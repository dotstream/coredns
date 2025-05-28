package azure_identity

import (
	"context"
	"strings"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/file"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

var log = clog.NewWithPlugin("azure_identity")

type AzureIdentity struct {
	resourceGroupName    string
	local                bool
	refreshDelayInSecond int64
	tenantId             string
	subscriptionId       string
	clientId             string
	clientSecret         string
	provider             *AzureProvider
	Next                 plugin.Handler
}

func (az AzureIdentity) Name() string { return "azure_identity" }

func (az AzureIdentity) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {

	state := request.Request{W: w, Req: r}
	qname := state.Name()

	zone := plugin.Zones(az.provider.zoneNames).Matches(qname)

	if zone == "" {
		log.Infof("%s is not part of our current zone %s.", qname, zone)
		return plugin.NextOrFailure(az.Name(), az.Next, ctx, w, r)
	}

	mz := az.provider.findZone(zone)

	if mz == nil {
		log.Errorf("Doesn't find the correct zone %s in the cache.", zone)
		return dns.RcodeServerFailure, nil
	}

	log.Infof("%s is requesting from our current zone %s.", qname, zone)

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	var result file.Result

	az.provider.zMu.RLock()
	m.Answer, m.Ns, m.Extra, result = mz.z.Lookup(ctx, state, qname)
	az.provider.zMu.RUnlock()

	if len(m.Answer) == 0 && result != file.NoData {
		log.Warningf("No data found for qname %s.", qname)
		return plugin.NextOrFailure(az.Name(), az.Next, ctx, w, r)
	}

	switch result {
	case file.Success:
	case file.NoData:
	case file.NameError:
		m.Rcode = dns.RcodeNameError
	case file.Delegation:
		m.Authoritative = false
	case file.ServerFailure:
		log.Infof("Request (%s) => result (ServerFailure)", qname)
		return dns.RcodeServerFailure, nil
	}

	answers := ""
	for _, a := range m.Answer {
		answers += a.String() + ","
	}
	answers = strings.TrimRight(answers, ",")

	log.Infof("Request (%s) => result (%d) , answer(%s).", qname, result, answers)

	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}
