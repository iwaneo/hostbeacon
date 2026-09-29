// Package discovery announces the Agent on the local network with mDNS
// (zeroconf), so Home Assistant can list it. Discovery is never trusted: it
// only offers to start Pairing, and Home Assistant moves a paired Host to a
// new address only after the certificate pin and the Pairing key match there.
package discovery

import (
	"context"

	"github.com/brutella/dnssd"
	dnssdlog "github.com/brutella/dnssd/log"
)

// ServiceType is the service the Agent announces. The Integration's manifest
// lists it too.
const ServiceType = "_hostbeacon._tcp"

// Announce answers mDNS queries for the Agent until ctx ends. name is shown in
// Home Assistant (the hostname). The TXT record "id" holds the instance ID,
// so Home Assistant can tell a Host it already shows. The addresses are read
// from the network interfaces at each answer, so a new address is announced
// without a restart.
func Announce(ctx context.Context, name, instanceID string, port int) error {
	dnssdlog.Info.Disable()
	service, err := dnssd.NewService(dnssd.Config{
		Name: name,
		Type: ServiceType,
		// Not the hostname: another mDNS responder on the Host (Avahi) owns
		// <hostname>.local, and the two would conflict.
		Host: "hostbeacon-" + instanceID[:8],
		Text: map[string]string{"id": instanceID},
		Port: port,
	})
	if err != nil {
		return err
	}
	responder, err := dnssd.NewResponder()
	if err != nil {
		return err
	}
	if _, err := responder.Add(service); err != nil {
		return err
	}
	return responder.Respond(ctx)
}
