// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Pyrinpyi/pyipad/app/appmessage"

	"github.com/Pyrinpyi/pyipad/domain/consensus/model/externalapi"
	"github.com/Pyrinpyi/pyipad/domain/consensus/utils/subnetworks"

	"github.com/Pyrinpyi/pyipad/infrastructure/network/dnsseed"
	"github.com/pkg/errors"

	"github.com/miekg/dns"
)

// DNSServer struct
type DNSServer struct {
	hostname   string
	listen     string
	nameserver string
}

// Start - starts server
func (d *DNSServer) Start() {
	defer wg.Done()

	rr := fmt.Sprintf("%s 86400 IN NS %s", d.hostname, d.nameserver)
	authority, err := dns.NewRR(rr)
	if err != nil {
		log.Infof("NewRR: %v", err)
		return
	}

	udpAddr, err := net.ResolveUDPAddr("udp4", d.listen)
	if err != nil {
		log.Infof("ResolveUDPAddr: %v", err)
		return
	}

	udpListen, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Infof("ListenUDP: %v", err)
		return
	}
	defer udpListen.Close()

	for {
		b := make([]byte, 512)
	mainLoop:
		err := udpListen.SetReadDeadline(time.Now().Add(time.Second))
		if err != nil {
			log.Infof("SetReadDeadline: %v", err)
			os.Exit(1)
		}
		_, addr, err := udpListen.ReadFromUDP(b)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if atomic.LoadInt32(&systemShutdown) == 0 {
					// use goto in order to do not re-allocate 'b' buffer
					goto mainLoop
				}
				log.Infof("DNS server shutdown")
				return
			}
			var opErr *net.OpError
			if errors.As(err, &opErr) {
				log.Infof("Read: %T", opErr.Err)
			} else {
				log.Errorf("Unknown error: %s", err)
			}
			continue
		}

		wg.Add(1)

		spawn("DNSServer.Start-DNSServer.handleDNSRequest",
			func() { d.handleDNSRequest(addr, authority, udpListen, b) })
	}
}

// NewDNSServer - create DNS server
func NewDNSServer(hostname, nameserver, listen string) *DNSServer {
	if hostname[len(hostname)-1] != '.' {
		hostname = hostname + "."
	}
	if nameserver[len(nameserver)-1] != '.' {
		nameserver = nameserver + "."
	}

	return &DNSServer{
		hostname:   hostname,
		listen:     listen,
		nameserver: nameserver,
	}
}

func (d *DNSServer) extractSubnetworkID(addr *net.UDPAddr, domainName string) (*externalapi.DomainSubnetworkID, bool, error) {
	// Domain name may be in following format:
	//   [n[subnetwork].]hostname
	// where connmgr.SubnetworkIDPrefixChar is a prefix
	var subnetworkID *externalapi.DomainSubnetworkID
	includeAllSubnetworks := true
	if d.hostname != domainName {
		labels := dns.SplitDomainName(domainName)
		if labels[0][0] == dnsseed.SubnetworkIDPrefixChar {
			includeAllSubnetworks = false
			if len(labels[0]) > 1 {
				subnetworkID, err := subnetworks.FromString(labels[0][1:])
				if err != nil {
					log.Infof("%s: subnetworkid.NewFromStr: %v", addr, err)
					return subnetworkID, includeAllSubnetworks, err
				}
			}
		}
	}
	return subnetworkID, includeAllSubnetworks, nil
}

func (d *DNSServer) validateDNSRequest(addr *net.UDPAddr, b []byte) (dnsMsg *dns.Msg, domainName string, atype string, err error) {
	dnsMsg = new(dns.Msg)
	err = dnsMsg.Unpack(b[:])
	if err != nil {
		log.Infof("%s: invalid dns message: %v", addr, err)
		return nil, "", "", err
	}
	if len(dnsMsg.Question) != 1 {
		str := fmt.Sprintf("%s sent more than 1 question: %d", addr, len(dnsMsg.Question))
		log.Infof("%s", str)
		return nil, "", "", errors.Errorf("%s", str)
	}
	domainName = strings.ToLower(dnsMsg.Question[0].Name)
	ff := strings.LastIndex(domainName, d.hostname)
	if ff < 0 {
		str := fmt.Sprintf("invalid name: %s", dnsMsg.Question[0].Name)
		log.Infof("%s", str)
		return nil, "", "", errors.Errorf("%s", str)
	}
	atype, err = translateDNSQuestion(addr, dnsMsg)
	return dnsMsg, domainName, atype, err
}

func translateDNSQuestion(addr *net.UDPAddr, dnsMsg *dns.Msg) (string, error) {
	var atype string
	qtype := dnsMsg.Question[0].Qtype
	switch qtype {
	case dns.TypeA:
		atype = "A"
	case dns.TypeAAAA:
		atype = "AAAA"
	case dns.TypeNS:
		atype = "NS"
	default:
		str := fmt.Sprintf("%s: invalid qtype: %d", addr, dnsMsg.Question[0].Qtype)
		log.Infof("%s", str)
		return "", errors.Errorf("%s", str)
	}
	return atype, nil
}

func (d *DNSServer) buildDNSResponse(addr *net.UDPAddr, authority dns.RR, dnsMsg *dns.Msg, includeAllSubnetworks bool,
	subnetworkID *externalapi.DomainSubnetworkID, atype string) ([]byte, error) {

	respMsg := dnsMsg.Copy()
	respMsg.Authoritative = true
	respMsg.Response = true

	qtype := dnsMsg.Question[0].Qtype
	if qtype != dns.TypeNS {
		respMsg.Ns = append(respMsg.Ns, authority)
		addrs := amgr.GoodAddresses(qtype, includeAllSubnetworks, subnetworkID)
		log.Infof("%s: Sending %d addresses", addr, len(addrs))
		if len(addrs) == 0 && qtype == dns.TypeAAAA {
			// Musl (Alpine) requires non-empty result (work-around):
			addrs = append(addrs, appmessage.NewNetAddressIPPort(net.ParseIP("100::"), uint16(0)))
		}
		for _, a := range addrs {
			rr := fmt.Sprintf("%s 30 IN %s %s", dnsMsg.Question[0].Name, atype, a.IP.String())
			newRR, err := dns.NewRR(rr)
			if err != nil {
				log.Infof("%s: NewRR: %v", addr, err)
				return nil, err
			}

			respMsg.Answer = append(respMsg.Answer, newRR)
		}
	} else {
		rr := fmt.Sprintf("%s 86400 IN NS %s", dnsMsg.Question[0].Name, d.nameserver)
		newRR, err := dns.NewRR(rr)
		if err != nil {
			log.Infof("%s: NewRR: %v", addr, err)
			return nil, err
		}

		respMsg.Answer = append(respMsg.Answer, newRR)
	}

	sendBytes, err := respMsg.Pack()
	if err != nil {
		log.Infof("%s: failed to pack response: %v", addr, err)
		return nil, err
	}
	return sendBytes, nil
}

func (d *DNSServer) handleDNSRequest(addr *net.UDPAddr, authority dns.RR, udpListen *net.UDPConn, b []byte) {
	defer wg.Done()

	dnsMsg, domainName, atype, err := d.validateDNSRequest(addr, b)
	if err != nil {
		return
	}

	subnetworkID, includeAllSubnetworks, err := d.extractSubnetworkID(addr, domainName)
	if err != nil {
		return
	}

	log.Infof("%s: query %d for subnetwork ID %v",
		addr, dnsMsg.Question[0].Qtype, subnetworkID)

	sendBytes, err := d.buildDNSResponse(addr, authority, dnsMsg, includeAllSubnetworks, subnetworkID, atype)
	if err != nil {
		return
	}

	_, err = udpListen.WriteToUDP(sendBytes, addr)
	if err != nil {
		log.Infof("%s: failed to write response: %v", addr, err)
		return
	}
}
