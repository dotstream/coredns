package azure_identity

import (
	"context"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
)

func init() {
	plugin.Register("azure_identity", setup)
}

func setup(c *caddy.Controller) error {
	azi, err := parse(c)
	if err != nil {
		return plugin.Error("azure_identity", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	provider, err := NewAzureProvider(azi.subscriptionId, azi.resourceGroupName, azi.tenantId)
	if err != nil {
		cancel()
		return plugin.Error("azure_identity", err)
	}
	azi.provider = provider

	if err := azi.provider.Run(ctx); err != nil {
		log.Error(err)
		cancel()
		return plugin.Error("azure_identity", err)
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		azi.Next = next
		return azi
	})

	c.OnShutdown(func() error { cancel(); return nil })

	return nil
}

func parse(c *caddy.Controller) (AzureIdentity, error) {

	azureIdentity := AzureIdentity{}

	for c.Next() {
		for c.NextBlock() {
			switch c.Val() {
			case "subscription":
				if !c.NextArg() {
					return azureIdentity, c.ArgErr()
				}
				azureIdentity.subscriptionId = c.Val()
			case "tenant":
				if !c.NextArg() {
					return azureIdentity, c.ArgErr()
				}
				azureIdentity.tenantId = c.Val()
			case "zone":
				if !c.NextArg() {
					return azureIdentity, c.ArgErr()
				}
				azureIdentity.dnsZone = c.Val()
			case "resource_group":
				if !c.NextArg() {
					return azureIdentity, c.ArgErr()
				}
				azureIdentity.resourceGroupName = c.Val()
			default:
				return azureIdentity, c.Errf("unknown property: %q", c.Val())
			}
		}
	}

	return azureIdentity, nil
}
