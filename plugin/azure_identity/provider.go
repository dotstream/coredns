package azure_identity

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	azcoreruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/coredns/coredns/plugin/file"
	"github.com/google/uuid"
	ddns "github.com/miekg/dns"
)

type ctxKey string

const (
	// Context key for request ID
	clientRequestIDKey ctxKey = "client-request-id"
	// Azure API Headers
	msRequestIDHeader          = "x-ms-request-id"
	msCorrelationRequestHeader = "x-ms-correlation-request-id"
	msClientRequestIDHeader    = "x-ms-client-request-id"
)

type customHeaderPolicy struct{}

func (p *customHeaderPolicy) Do(req *policy.Request) (*http.Response, error) {
	id := req.Raw().Header.Get(msClientRequestIDHeader)
	if id == "" {
		id = uuid.New().String()
		req.Raw().Header.Set(msClientRequestIDHeader, id)
		newCtx := context.WithValue(req.Raw().Context(), clientRequestIDKey, id)
		*req.Raw() = *req.Raw().WithContext(newCtx)
	}
	return req.Next()
}
func CustomHeaderPolicynew() policy.Policy { return &customHeaderPolicy{} }

const (
	defaultTTL = 300
)

// ZonesClient is an interface of dns.ZoneClient that can be stubbed for testing.
type ZonesClient interface {
	NewListByResourceGroupPager(resourceGroupName string, options *dns.ZonesClientListByResourceGroupOptions) *azcoreruntime.Pager[dns.ZonesClientListByResourceGroupResponse]
}

// RecordSetsClient is an interface of dns.RecordSetsClient that can be stubbed for testing.
type RecordSetsClient interface {
	NewListAllByDNSZonePager(resourceGroupName string, zoneName string, options *dns.RecordSetsClientListAllByDNSZoneOptions) *azcoreruntime.Pager[dns.RecordSetsClientListAllByDNSZoneResponse]
	Delete(ctx context.Context, resourceGroupName string, zoneName string, relativeRecordSetName string, recordType dns.RecordType, options *dns.RecordSetsClientDeleteOptions) (dns.RecordSetsClientDeleteResponse, error)
	CreateOrUpdate(ctx context.Context, resourceGroupName string, zoneName string, relativeRecordSetName string, recordType dns.RecordType, parameters dns.RecordSet, options *dns.RecordSetsClientCreateOrUpdateOptions) (dns.RecordSetsClientCreateOrUpdateResponse, error)
}

type myZone struct {
	dnsZone dns.Zone
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

func NewAzureProvider(subscriptionID string, resourceGroup string, tenantID string) (*AzureProvider, error) {

	cloudCfg := cloud.AzurePublic
	clientOpts := azcore.ClientOptions{
		Cloud: cloudCfg,
		Retry: policy.RetryOptions{
			MaxRetries: 0,
			TryTimeout: 5000,
		},
		Logging: policy.LogOptions{
			AllowedHeaders: []string{
				msRequestIDHeader,
				msCorrelationRequestHeader,
				msClientRequestIDHeader,
			},
		},
		PerCallPolicies: []policy.Policy{
			CustomHeaderPolicynew(),
		},
	}

	log.Infof("Configured Azure client with maxRetries: %d", clientOpts.Retry.MaxRetries)
	armClientOpts := &arm.ClientOptions{
		ClientOptions: clientOpts,
	}
	log.Info("Using managed identity extension to retrieve access token for Azure API.")
	msiOpt := azidentity.ManagedIdentityCredentialOptions{
		ClientOptions: clientOpts,
	}

	cred, err := azidentity.NewManagedIdentityCredential(&msiOpt)
	if err != nil {
		return nil, err
	}

	zonesClient, err := dns.NewZonesClient(subscriptionID, cred, armClientOpts)

	if err != nil {
		return nil, err
	}

	recordSetsClient, err := dns.NewRecordSetsClient(subscriptionID, cred, armClientOpts)

	if err != nil {
		return nil, err
	}

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

	pager := az.zonesClient.NewListByResourceGroupPager(az.resourceGroup, &dns.ZonesClientListByResourceGroupOptions{Top: nil})
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

func updateZoneFromPublicResourceSet(recordSet *azcoreruntime.Pager[dns.RecordSetsClientListAllByDNSZoneResponse], newZ *file.Zone) error {
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
					a := &ddns.A{
						Hdr: ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeA, Class: ddns.ClassINET, Ttl: resultTTL},
						A:   net.ParseIP(*(A.IPv4Address)),
					}
					newZ.Insert(a)
				}
			}

			if v.Properties.AaaaRecords != nil {
				for _, AAAA := range v.Properties.AaaaRecords {
					aaaa := &ddns.AAAA{
						Hdr:  ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeAAAA, Class: ddns.ClassINET, Ttl: resultTTL},
						AAAA: net.ParseIP(*(AAAA.IPv6Address)),
					}
					newZ.Insert(aaaa)
				}
			}

			if v.Properties.MxRecords != nil {
				for _, MX := range v.Properties.MxRecords {
					mx := &ddns.MX{
						Hdr:        ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeMX, Class: ddns.ClassINET, Ttl: resultTTL},
						Preference: uint16(*(MX.Preference)),
						Mx:         ddns.Fqdn(*(MX.Exchange)),
					}
					newZ.Insert(mx)
				}
			}

			if v.Properties.PtrRecords != nil {
				for _, PTR := range v.Properties.PtrRecords {
					ptr := &ddns.PTR{
						Hdr: ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypePTR, Class: ddns.ClassINET, Ttl: resultTTL},
						Ptr: ddns.Fqdn(*(PTR.Ptrdname)),
					}
					newZ.Insert(ptr)
				}
			}

			if v.Properties.SrvRecords != nil {
				for _, SRV := range v.Properties.SrvRecords {
					srv := &ddns.SRV{
						Hdr:      ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeSRV, Class: ddns.ClassINET, Ttl: resultTTL},
						Priority: uint16(*(SRV.Priority)),
						Weight:   uint16(*(SRV.Weight)),
						Port:     uint16(*(SRV.Port)),
						Target:   ddns.Fqdn(*(SRV.Target)),
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
					txt := &ddns.TXT{
						Hdr: ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeTXT, Class: ddns.ClassINET, Ttl: resultTTL},
						Txt: strings,
					}
					newZ.Insert(txt)
				}
			}

			if v.Properties.NsRecords != nil {
				for _, NS := range v.Properties.NsRecords {
					ns := &ddns.NS{
						Hdr: ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeNS, Class: ddns.ClassINET, Ttl: resultTTL},
						Ns:  *(NS.Nsdname),
					}
					newZ.Insert(ns)
				}
			}

			if v.Properties.SoaRecord != nil {
				SOA := v.Properties.SoaRecord
				soa := &ddns.SOA{
					Hdr:     ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeSOA, Class: ddns.ClassINET, Ttl: resultTTL},
					Minttl:  uint32(*(SOA.MinimumTTL)),
					Expire:  uint32(*(SOA.ExpireTime)),
					Retry:   uint32(*(SOA.RetryTime)),
					Refresh: uint32(*(SOA.RefreshTime)),
					Serial:  uint32(*(SOA.SerialNumber)),
					Mbox:    ddns.Fqdn(*(SOA.Email)),
					Ns:      *(SOA.Host),
				}
				newZ.Insert(soa)
			}

			if v.Properties.CnameRecord != nil {
				CNAME := v.Properties.CnameRecord.Cname
				cname := &ddns.CNAME{
					Hdr:    ddns.RR_Header{Name: resultFqdn, Rrtype: ddns.TypeCNAME, Class: ddns.ClassINET, Ttl: resultTTL},
					Target: ddns.Fqdn(*CNAME),
				}
				newZ.Insert(cname)
			}
		}
	}

	return nil
}
