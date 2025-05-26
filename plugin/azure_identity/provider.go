package azure_identity

import (
	"context"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcoreruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	azuredns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/coredns/coredns/plugin/file"
	"github.com/miekg/dns"
)

// ZonesClient is an interface of dns.ZoneClient that can be stubbed for testing.
type ZonesClient interface {
	NewListByResourceGroupPager(resourceGroupName string, options *azuredns.ZonesClientListByResourceGroupOptions) *azcoreruntime.Pager[azuredns.ZonesClientListByResourceGroupResponse]
}

// RecordSetsClient is an interface of dns.RecordSetsClient that can be stubbed for testing.
type RecordSetsClient interface {
	NewListAllByDNSZonePager(resourceGroupName string, zoneName string, options *azuredns.RecordSetsClientListAllByDNSZoneOptions) *azcoreruntime.Pager[azuredns.RecordSetsClientListAllByDNSZoneResponse]
	Delete(ctx context.Context, resourceGroupName string, zoneName string, relativeRecordSetName string, recordType azuredns.RecordType, options *azuredns.RecordSetsClientDeleteOptions) (azuredns.RecordSetsClientDeleteResponse, error)
	CreateOrUpdate(ctx context.Context, resourceGroupName string, zoneName string, relativeRecordSetName string, recordType azuredns.RecordType, parameters azuredns.RecordSet, options *azuredns.RecordSetsClientCreateOrUpdateOptions) (azuredns.RecordSetsClientCreateOrUpdateResponse, error)
}

type myZone struct {
	dnsZone azuredns.Zone
	z       *file.Zone
	name    string
}

type AzureProvider struct {
	zoneNames        []string
	resourceGroup    string
	zonesClient      ZonesClient
	recordSetsClient RecordSetsClient
	maxRetriesCount  int
	zonesCache       *zonesCache[myZone]
}

func getAuthorization(clientId, clientSecret, tenantId string) (azcore.TokenCredential, error) {

	if clientId != "" && clientSecret != "" {
		log.Infof("Using authenticating with clientID and secret key")
		cred, err := azidentity.NewClientSecretCredential(tenantId, clientId, clientSecret, nil)
		if err != nil {
			return nil, err
		}
		return cred, nil
	}

	// Use Workload Identity if present
	if os.Getenv("AZURE_FEDERATED_TOKEN_FILE") != "" {

		wcOpt := azidentity.WorkloadIdentityCredentialOptions{}

		if clientId != "" {
			wcOpt.ClientID = clientId
		}

		if tenantId != "" {
			wcOpt.TenantID = tenantId
		}

		return azidentity.NewWorkloadIdentityCredential(&wcOpt)
	}

	log.Info("No Azure Workload Identity found: attempting to authenticate with an Azure Managed Service Identity (MSI)")

	msiOpt := &azidentity.ManagedIdentityCredentialOptions{}
	if clientId != "" {
		msiOpt.ID = azidentity.ClientID(clientId)
	}

	cred, err := azidentity.NewManagedIdentityCredential(msiOpt)
	if err != nil {
		return nil, err
	}

	return cred, nil
}

func NewAzureProvider(subscriptionID string, resourceGroup string, tenantID string, clientId string, clientSecret string) (*AzureProvider, error) {

	log.Info("Configured Azure client")

	cred, err := getAuthorization(clientId, clientSecret, tenantID)

	if err != nil {
		return nil, err
	}

	factory, err := azuredns.NewClientFactory(subscriptionID, cred, nil)

	if err != nil {
		return nil, err
	}

	zonesClient := factory.NewZonesClient()
	recordSetsClient := factory.NewRecordSetsClient()

	return &AzureProvider{
		resourceGroup:    resourceGroup,
		zonesClient:      zonesClient,
		zonesCache:       &zonesCache[myZone]{duration: time.Duration(time.Duration.Minutes(5))}, // 5 minutes zone cache
		recordSetsClient: recordSetsClient,
		maxRetriesCount:  10,
	}, nil
}

func (p *AzureProvider) Run(ctx context.Context) error {

	if err := p.updateZones(ctx); err != nil {
		return err
	}
	go func() {
		delay := 1 * time.Minute
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			timer.Reset(delay)
			select {
			case <-ctx.Done():
				log.Debugf("Breaking out of Azure update loop for %v: %v", p.zoneNames, ctx.Err())
				return
			case <-timer.C:
				if err := p.updateRecords(ctx); err != nil && ctx.Err() == nil {
					log.Errorf("Failed to update records %v: %v", p.zoneNames, err)
				}
			}
		}
	}()
	return nil
}

func (az *AzureProvider) findZone(zoneName string) *myZone {

	for _, zone := range az.zonesCache.Get() {
		if zone.name == zoneName {
			return &zone
		}
	}

	return nil
}

func (az *AzureProvider) updateZones(ctx context.Context) error {

	log.Infof("Retrieving Azure DNS zones for resource group: %s.", az.resourceGroup)

	pager := az.zonesClient.NewListByResourceGroupPager(az.resourceGroup, &azuredns.ZonesClientListByResourceGroupOptions{Top: nil})
	var zones []myZone
	var zonesName []string

	for pager.More() {
		nextResult, err := pager.NextPage(ctx)

		if err != nil {
			return err
		}
		for _, zone := range nextResult.Value {
			if zone.Name != nil {
				log.Infof("Zone name %s.", *zone.Name)
				zones = append(zones, myZone{dnsZone: *zone, name: *zone.Name, z: nil})
				zonesName = append(zonesName, *zone.Name)
			}
		}
	}

	log.Infof("Found %d Azure DNS zone(s). Updating zones cache", len(zones))
	az.zonesCache.Reset(zones)
	az.zoneNames = zonesName

	return nil
}

func (az *AzureProvider) updateRecords(ctx context.Context) error {

	if az.zonesCache.Expired() {
		err := az.updateZones(ctx)
		if err != nil {
			return err
		}
	}

	for _, zone := range az.zonesCache.Get() {

		log.Infof("Retrieving Azure DNS records for resource group: %s and zone: %s.", az.resourceGroup, zone.name)

		newZ := file.NewZone(zone.name, "")
		zoneName := strings.TrimSuffix(zone.name, ".")

		err := updateZoneFromPublicResourceSet(az.recordSetsClient.NewListAllByDNSZonePager(az.resourceGroup, zoneName, nil), newZ)

		if err != nil {
			return err
		}

		zone.z = newZ

		log.Infof("Found %d Azure DNS records (zone: %s)", newZ.Tree.Count, zone.name)
	}

	return nil
}

func updateZoneFromPublicResourceSet(recordSet *azcoreruntime.Pager[azuredns.RecordSetsClientListAllByDNSZoneResponse], newZ *file.Zone) error {
	ctx := context.Background()

	for recordSet.More() {
		page, err := recordSet.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, v := range page.Value {
			resultFqdn := *v.Properties.Fqdn
			// TODO(vijayt): Azure TTL is int64 but below it expects uint32
			// The maximum value for the TTL can be 2,147,483,647 and
			// the maximum that a uint32 can hold is 4,294,967,295 so this should be ok but check with the maintainers
			resultTTL := uint32(*v.Properties.TTL)
			if v.Properties.ARecords != nil {
				for _, A := range v.Properties.ARecords {
					a := &dns.A{
						Hdr: dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: resultTTL},
						A:   net.ParseIP(*(A.IPv4Address)),
					}
					newZ.Insert(a)
				}
			}

			if v.Properties.AaaaRecords != nil {
				for _, AAAA := range v.Properties.AaaaRecords {
					aaaa := &dns.AAAA{
						Hdr:  dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: resultTTL},
						AAAA: net.ParseIP(*(AAAA.IPv6Address)),
					}
					newZ.Insert(aaaa)
				}
			}

			if v.Properties.MxRecords != nil {
				for _, MX := range v.Properties.MxRecords {
					mx := &dns.MX{
						Hdr:        dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: resultTTL},
						Preference: uint16(*(MX.Preference)),
						Mx:         dns.Fqdn(*(MX.Exchange)),
					}
					newZ.Insert(mx)
				}
			}

			if v.Properties.PtrRecords != nil {
				for _, PTR := range v.Properties.PtrRecords {
					ptr := &dns.PTR{
						Hdr: dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: resultTTL},
						Ptr: dns.Fqdn(*(PTR.Ptrdname)),
					}
					newZ.Insert(ptr)
				}
			}

			if v.Properties.SrvRecords != nil {
				for _, SRV := range v.Properties.SrvRecords {
					srv := &dns.SRV{
						Hdr:      dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: resultTTL},
						Priority: uint16(*(SRV.Priority)),
						Weight:   uint16(*(SRV.Weight)),
						Port:     uint16(*(SRV.Port)),
						Target:   dns.Fqdn(*(SRV.Target)),
					}
					newZ.Insert(srv)
				}
			}

			if v.Properties.TxtRecords != nil {
				for _, TXT := range v.Properties.TxtRecords {
					var strings []string
					for _, ptr := range TXT.Value {
						if ptr != nil {
							strings = append(strings, *ptr)
						}
					}
					txt := &dns.TXT{
						Hdr: dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: resultTTL},
						Txt: strings,
					}
					newZ.Insert(txt)
				}
			}

			if v.Properties.NsRecords != nil {
				for _, NS := range v.Properties.NsRecords {
					ns := &dns.NS{
						Hdr: dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: resultTTL},
						Ns:  *(NS.Nsdname),
					}
					newZ.Insert(ns)
				}
			}

			if v.Properties.SoaRecord != nil {
				SOA := v.Properties.SoaRecord
				soa := &dns.SOA{
					Hdr:     dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: resultTTL},
					Minttl:  uint32(*(SOA.MinimumTTL)),
					Expire:  uint32(*(SOA.ExpireTime)),
					Retry:   uint32(*(SOA.RetryTime)),
					Refresh: uint32(*(SOA.RefreshTime)),
					Serial:  uint32(*(SOA.SerialNumber)),
					Mbox:    dns.Fqdn(*(SOA.Email)),
					Ns:      *(SOA.Host),
				}
				newZ.Insert(soa)
			}

			if v.Properties.CnameRecord != nil {
				CNAME := v.Properties.CnameRecord.Cname
				cname := &dns.CNAME{
					Hdr:    dns.RR_Header{Name: resultFqdn, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: resultTTL},
					Target: dns.Fqdn(*CNAME),
				}
				newZ.Insert(cname)
			}
		}
	}

	return nil
}
