package acl

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	log "github.com/sirupsen/logrus"

	nberrors "github.com/netbirdio/netbird/client/errors"
	firewall "github.com/netbirdio/netbird/client/firewall/manager"
	"github.com/netbirdio/netbird/client/internal/acl/id"
	"github.com/netbirdio/netbird/client/ssh"
	"github.com/netbirdio/netbird/management/domain"
	mgmProto "github.com/netbirdio/netbird/management/proto"
)

var ErrSourceRangesEmpty = errors.New("sources range is empty")

// Manager is a ACL rules manager
type Manager interface {
	ApplyFiltering(networkMap *mgmProto.NetworkMap, dnsRouteFeatureFlag bool)
}

type protoMatch struct {
	ips      map[string]int
	policyID []byte
}

// DefaultManager uses firewall manager to handle
type DefaultManager struct {
	firewall       firewall.Manager
	ipsetCounter   int
	peerRulesPairs map[id.RuleID][]firewall.Rule
	routeRules     map[id.RuleID]struct{}
	mutex          sync.Mutex
}

func NewDefaultManager(fm firewall.Manager) *DefaultManager {
	return &DefaultManager{
		firewall:       fm,
		peerRulesPairs: make(map[id.RuleID][]firewall.Rule),
		routeRules:     make(map[id.RuleID]struct{}),
	}
}

// ApplyFiltering firewall rules to the local firewall manager processed by ACL policy.
//
// If allowByDefault is true it appends allow ALL traffic rules to input and output chains.
func (d *DefaultManager) ApplyFiltering(networkMap *mgmProto.NetworkMap, dnsRouteFeatureFlag bool) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	if d.firewall == nil {
		log.Debug("firewall manager is not supported, skipping firewall rules")
		return
	}

	start := time.Now()
	defer func() {
		total := 0
		for _, pairs := range d.peerRulesPairs {
			total += len(pairs)
		}
		log.Infof(
			"ACL rules processed in: %v, total rules count: %d",
			time.Since(start), total)
	}()

	d.applyPeerACLs(networkMap)

	if err := d.applyRouteACLs(networkMap.RoutesFirewallRules, dnsRouteFeatureFlag); err != nil {
		log.Errorf("Failed to apply route ACLs: %v", err)
	}

	if err := d.firewall.Flush(); err != nil {
		log.Error("failed to flush firewall rules: ", err)
	}
}

func (d *DefaultManager) applyPeerACLs(networkMap *mgmProto.NetworkMap) {
	rules, squashedProtocols := d.squashAcceptRules(networkMap)

	enableSSH := networkMap.PeerConfig != nil &&
		networkMap.PeerConfig.SshConfig != nil &&
		networkMap.PeerConfig.SshConfig.SshEnabled
	if _, ok := squashedProtocols[mgmProto.RuleProtocol_ALL]; ok {
		enableSSH = enableSSH && !ok
	}
	if _, ok := squashedProtocols[mgmProto.RuleProtocol_TCP]; ok {
		enableSSH = enableSSH && !ok
	}

	// if TCP protocol rules not squashed and SSH enabled
	// we add default firewall rule which accepts connection to any peer
	// in the network by SSH (TCP 22 port).
	if enableSSH {
		rules = append(rules, &mgmProto.FirewallRule{
			PeerIP:    "0.0.0.0",
			Direction: mgmProto.RuleDirection_IN,
			Action:    mgmProto.RuleAction_ACCEPT,
			Protocol:  mgmProto.RuleProtocol_TCP,
			Port:      strconv.Itoa(ssh.DefaultSSHPort),
		})
	}

	// if we got empty rules list but management not set networkMap.FirewallRulesIsEmpty flag
	// we have old version of management without rules handling, we should allow all traffic
	if len(networkMap.FirewallRules) == 0 && !networkMap.FirewallRulesIsEmpty {
		log.Warn("this peer is connected to a NetBird Management service with an older version. Allowing all traffic from connected peers")
		rules = append(rules,
			&mgmProto.FirewallRule{
				PeerIP:    "0.0.0.0",
				Direction: mgmProto.RuleDirection_IN,
				Action:    mgmProto.RuleAction_ACCEPT,
				Protocol:  mgmProto.RuleProtocol_ALL,
			},
			&mgmProto.FirewallRule{
				PeerIP:    "0.0.0.0",
				Direction: mgmProto.RuleDirection_OUT,
				Action:    mgmProto.RuleAction_ACCEPT,
				Protocol:  mgmProto.RuleProtocol_ALL,
			},
		)
	}

	newRulePairs := make(map[id.RuleID][]firewall.Rule)
	ipsetByRuleSelectors := make(map[string]string)

	for _, r := range rules {
		// if this rule is member of rule selection with more than DefaultIPsCountForSet
		// it's IP address can be used in the ipset for firewall manager which supports it
		selector := d.getRuleGroupingSelector(r)
		ipsetName, ok := ipsetByRuleSelectors[selector]
		if !ok {
			d.ipsetCounter++
			ipsetName = fmt.Sprintf("nb%07d", d.ipsetCounter)
			ipsetByRuleSelectors[selector] = ipsetName
		}
		pairID, rulePair, err := d.protoRuleToFirewallRule(r, ipsetName)
		if err != nil {
			log.Errorf("failed to apply firewall rule: %+v, %v", r, err)
			d.rollBack(newRulePairs)
			break
		}
		if len(rulePair) > 0 {
			d.peerRulesPairs[pairID] = rulePair
			newRulePairs[pairID] = rulePair
		}
	}

	for pairID, rules := range d.peerRulesPairs {
		if _, ok := newRulePairs[pairID]; !ok {
			for _, rule := range rules {
				if err := d.firewall.DeletePeerRule(rule); err != nil {
					log.Errorf("failed to delete peer firewall rule: %v", err)
					continue
				}
			}
			delete(d.peerRulesPairs, pairID)
		}
	}
	d.peerRulesPairs = newRulePairs
}

func (d *DefaultManager) applyRouteACLs(rules []*mgmProto.RouteFirewallRule, dynamicResolver bool) error {
	newRouteRules := make(map[id.RuleID]struct{}, len(rules))
	var merr *multierror.Error

	// Apply new rules - firewall manager will return existing rule ID if already present
	for _, rule := range rules {
		id, err := d.applyRouteACL(rule, dynamicResolver)
		if err != nil {
			if errors.Is(err, ErrSourceRangesEmpty) {
				log.Debugf("skipping empty sources rule with destination %s: %v", rule.Destination, err)
			} else {
				merr = multierror.Append(merr, fmt.Errorf("add route rule: %w", err))
			}
			continue
		}
		newRouteRules[id] = struct{}{}
	}

	// Clean up old firewall rules
	for id := range d.routeRules {
		if _, exists := newRouteRules[id]; !exists {
			if err := d.firewall.DeleteRouteRule(id); err != nil {
				merr = multierror.Append(merr, fmt.Errorf("delete route rule: %w", err))
			}
			// implicitly deleted from the map
		}
	}

	d.routeRules = newRouteRules
	return nberrors.FormatErrorOrNil(merr)
}

func (d *DefaultManager) applyRouteACL(rule *mgmProto.RouteFirewallRule, dynamicResolver bool) (id.RuleID, error) {
	if len(rule.SourceRanges) == 0 {
		return "", ErrSourceRangesEmpty
	}

	var sources []netip.Prefix
	for _, sourceRange := range rule.SourceRanges {
		source, err := netip.ParsePrefix(sourceRange)
		if err != nil {
			return "", fmt.Errorf("parse source range: %w", err)
		}
		sources = append(sources, source)
	}

	destination, err := determineDestination(rule, dynamicResolver, sources)
	if err != nil {
		return "", fmt.Errorf("determine destination: %w", err)
	}

	protocol, err := convertToFirewallProtocol(rule.Protocol)
	if err != nil {
		return "", fmt.Errorf("invalid protocol: %w", err)
	}

	action, err := convertFirewallAction(rule.Action)
	if err != nil {
		return "", fmt.Errorf("invalid action: %w", err)
	}

	dPorts := convertPortInfo(rule.PortInfo)

	addedRule, err := d.firewall.AddRouteFiltering(rule.PolicyID, sources, destination, protocol, nil, dPorts, action)
	if err != nil {
		return "", fmt.Errorf("add route rule: %w", err)
	}

	return id.RuleID(addedRule.ID()), nil
}

func (d *DefaultManager) protoRuleToFirewallRule(
	r *mgmProto.FirewallRule,
	ipsetName string,
) (id.RuleID, []firewall.Rule, error) {
	ip := net.ParseIP(r.PeerIP)
	if ip == nil {
		return "", nil, fmt.Errorf("invalid IP address, skipping firewall rule")
	}

	protocol, err := convertToFirewallProtocol(r.Protocol)
	if err != nil {
		return "", nil, fmt.Errorf("skipping firewall rule: %s", err)
	}

	action, err := convertFirewallAction(r.Action)
	if err != nil {
		return "", nil, fmt.Errorf("skipping firewall rule: %s", err)
	}

	var port *firewall.Port
	if !portInfoEmpty(r.PortInfo) {
		port = convertPortInfo(r.PortInfo)
	} else if r.Port != "" {
		// old version of management, single port
		value, err := strconv.Atoi(r.Port)
		if err != nil {
			return "", nil, fmt.Errorf("invalid port: %w", err)
		}
		port = &firewall.Port{
			Values: []uint16{uint16(value)},
		}
	}

	ruleID := d.getPeerRuleID(ip, protocol, int(r.Direction), port, action)
	if rulesPair, ok := d.peerRulesPairs[ruleID]; ok {
		return ruleID, rulesPair, nil
	}

	var rules []firewall.Rule
	switch r.Direction {
	case mgmProto.RuleDirection_IN:
		rules, err = d.addInRules(r.PolicyID, ip, protocol, port, action, ipsetName)
	case mgmProto.RuleDirection_OUT:
		if d.firewall.IsStateful() {
			return "", nil, nil
		}
		// return traffic for outbound connections if firewall is stateless
		rules, err = d.addOutRules(r.PolicyID, ip, protocol, port, action, ipsetName)
	default:
		return "", nil, fmt.Errorf("invalid direction, skipping firewall rule")
	}

	if err != nil {
		return "", nil, err
	}

	return ruleID, rules, nil
}

func portInfoEmpty(portInfo *mgmProto.PortInfo) bool {
	if portInfo == nil {
		return true
	}

	switch portInfo.GetPortSelection().(type) {
	case *mgmProto.PortInfo_Port:
		return portInfo.GetPort() == 0
	case *mgmProto.PortInfo_Range_:
		r := portInfo.GetRange()
		return r == nil || r.Start == 0 || r.End == 0
	default:
		return true
	}
}

func (d *DefaultManager) addInRules(
	id []byte,
	ip net.IP,
	protocol firewall.Protocol,
	port *firewall.Port,
	action firewall.Action,
	ipsetName string,
) ([]firewall.Rule, error) {
	rule, err := d.firewall.AddPeerFiltering(id, ip, protocol, nil, port, action, ipsetName)
	if err != nil {
		return nil, fmt.Errorf("add firewall rule: %w", err)
	}

	return rule, nil
}

func (d *DefaultManager) addOutRules(
	id []byte,
	ip net.IP,
	protocol firewall.Protocol,
	port *firewall.Port,
	action firewall.Action,
	ipsetName string,
) ([]firewall.Rule, error) {
	if shouldSkipInvertedRule(protocol, port) {
		return nil, nil
	}

	rule, err := d.firewall.AddPeerFiltering(id, ip, protocol, port, nil, action, ipsetName)
	if err != nil {
		return nil, fmt.Errorf("add firewall rule: %w", err)
	}

	return rule, nil
}

// getPeerRuleID() returns unique ID for the rule based on its parameters.
func (d *DefaultManager) getPeerRuleID(
	ip net.IP,
	proto firewall.Protocol,
	direction int,
	port *firewall.Port,
	action firewall.Action,
) id.RuleID {
	idStr := ip.String() + string(proto) + strconv.Itoa(direction) + strconv.Itoa(int(action))
	if port != nil {
		idStr += port.String()
	}

	return id.RuleID(hex.EncodeToString(md5.New().Sum([]byte(idStr))))
}

// squashAcceptRules does complex logic to convert many rules which allows connection by traffic type
// to all peers in the network map to one rule which just accepts that type of the traffic.
//
// NOTE: It will not squash two rules for same protocol if one covers all peers in the network,
// but other has port definitions or has drop policy.
func (d *DefaultManager) squashAcceptRules(
	networkMap *mgmProto.NetworkMap,
) ([]*mgmProto.FirewallRule, map[mgmProto.RuleProtocol]struct{}) {
	totalIPs := 0
	for _, p := range append(networkMap.RemotePeers, networkMap.OfflinePeers...) {
		for range p.AllowedIps {
			totalIPs++
		}
	}

	in := map[mgmProto.RuleProtocol]*protoMatch{}
	out := map[mgmProto.RuleProtocol]*protoMatch{}

	// trace which type of protocols was squashed
	squashedRules := []*mgmProto.FirewallRule{}
	squashedProtocols := map[mgmProto.RuleProtocol]struct{}{}

	// this function we use to do calculation, can we squash the rules by protocol or not.
	// We summ amount of Peers IP for given protocol we found in original rules list.
	// But we zeroed the IP's for protocol if:
	// 1. Any of the rule has DROP action type.
	// 2. Any of rule contains Port.
	//
	// We zeroed this to notify squash function that this protocol can't be squashed.
	addRuleToCalculationMap := func(i int, r *mgmProto.FirewallRule, protocols map[mgmProto.RuleProtocol]*protoMatch) {
		hasPortRestrictions := r.Action == mgmProto.RuleAction_DROP ||
			r.Port != "" || !portInfoEmpty(r.PortInfo)

		if hasPortRestrictions {
			// Don't squash rules with port restrictions
			protocols[r.Protocol] = &protoMatch{ips: map[string]int{}}
			return
		}

		if _, ok := protocols[r.Protocol]; !ok {
			protocols[r.Protocol] = &protoMatch{
				ips: map[string]int{},
				// store the first encountered PolicyID for this protocol
				policyID: r.PolicyID,
			}
		}

		// special case, when we receive this all network IP address
		// it means that rules for that protocol was already optimized on the
		// management side
		if r.PeerIP == "0.0.0.0" {
			squashedRules = append(squashedRules, r)
			squashedProtocols[r.Protocol] = struct{}{}
			return
		}

		ipset := protocols[r.Protocol].ips

		if _, ok := ipset[r.PeerIP]; ok {
			return
		}
		ipset[r.PeerIP] = i
	}

	for i, r := range networkMap.FirewallRules {
		// calculate squash for different directions
		if r.Direction == mgmProto.RuleDirection_IN {
			addRuleToCalculationMap(i, r, in)
		} else {
			addRuleToCalculationMap(i, r, out)
		}
	}

	// order of squashing by protocol is important
	// only for their first element ALL, it must be done first
	protocolOrders := []mgmProto.RuleProtocol{
		mgmProto.RuleProtocol_ALL,
		mgmProto.RuleProtocol_ICMP,
		mgmProto.RuleProtocol_TCP,
		mgmProto.RuleProtocol_UDP,
	}

	squash := func(matches map[mgmProto.RuleProtocol]*protoMatch, direction mgmProto.RuleDirection) {
		for _, protocol := range protocolOrders {
			match, ok := matches[protocol]
			if !ok || len(match.ips) != totalIPs || len(match.ips) < 2 {
				// don't squash if :
				// 1. Rules not cover all peers in the network
				// 2. Rules cover only one peer in the network.
				continue
			}

			// add special rule 0.0.0.0 which allows all IP's in our firewall implementations
			squashedRules = append(squashedRules, &mgmProto.FirewallRule{
				PeerIP:    "0.0.0.0",
				Direction: direction,
				Action:    mgmProto.RuleAction_ACCEPT,
				Protocol:  protocol,
				PolicyID:  match.policyID,
			})
			squashedProtocols[protocol] = struct{}{}

			if protocol == mgmProto.RuleProtocol_ALL {
				// if we have ALL traffic type squashed rule
				// it allows all other type of traffic, so we can stop processing
				break
			}
		}
	}

	squash(in, mgmProto.RuleDirection_IN)
	squash(out, mgmProto.RuleDirection_OUT)

	// if all protocol was squashed everything is allow and we can ignore all other rules
	if _, ok := squashedProtocols[mgmProto.RuleProtocol_ALL]; ok {
		return squashedRules, squashedProtocols
	}

	if len(squashedRules) == 0 {
		return networkMap.FirewallRules, squashedProtocols
	}

	var rules []*mgmProto.FirewallRule
	// filter out rules which was squashed from final list
	// if we also have other not squashed rules.
	for i, r := range networkMap.FirewallRules {
		if _, ok := squashedProtocols[r.Protocol]; ok {
			if m, ok := in[r.Protocol]; ok && m.ips[r.PeerIP] == i {
				continue
			} else if m, ok := out[r.Protocol]; ok && m.ips[r.PeerIP] == i {
				continue
			}
		}
		rules = append(rules, r)
	}

	return append(rules, squashedRules...), squashedProtocols
}

// getRuleGroupingSelector takes all rule properties except IP address to build selector
func (d *DefaultManager) getRuleGroupingSelector(rule *mgmProto.FirewallRule) string {
	return fmt.Sprintf("%v:%v:%v:%s:%v", strconv.Itoa(int(rule.Direction)), rule.Action, rule.Protocol, rule.Port, rule.PortInfo)
}

func (d *DefaultManager) rollBack(newRulePairs map[id.RuleID][]firewall.Rule) {
	log.Debugf("rollback ACL to previous state")
	for _, rules := range newRulePairs {
		for _, rule := range rules {
			if err := d.firewall.DeletePeerRule(rule); err != nil {
				log.Errorf("failed to delete new firewall rule (id: %v) during rollback: %v", rule.ID(), err)
			}
		}
	}
}

func convertToFirewallProtocol(protocol mgmProto.RuleProtocol) (firewall.Protocol, error) {
	switch protocol {
	case mgmProto.RuleProtocol_TCP:
		return firewall.ProtocolTCP, nil
	case mgmProto.RuleProtocol_UDP:
		return firewall.ProtocolUDP, nil
	case mgmProto.RuleProtocol_ICMP:
		return firewall.ProtocolICMP, nil
	case mgmProto.RuleProtocol_ALL:
		return firewall.ProtocolALL, nil
	default:
		return firewall.ProtocolALL, fmt.Errorf("invalid protocol type: %s", protocol.String())
	}
}

func shouldSkipInvertedRule(protocol firewall.Protocol, port *firewall.Port) bool {
	return protocol == firewall.ProtocolALL || protocol == firewall.ProtocolICMP || port == nil
}

func convertFirewallAction(action mgmProto.RuleAction) (firewall.Action, error) {
	switch action {
	case mgmProto.RuleAction_ACCEPT:
		return firewall.ActionAccept, nil
	case mgmProto.RuleAction_DROP:
		return firewall.ActionDrop, nil
	default:
		return firewall.ActionDrop, fmt.Errorf("invalid action type: %d", action)
	}
}

func convertPortInfo(portInfo *mgmProto.PortInfo) *firewall.Port {
	if portInfo == nil {
		return nil
	}

	if portInfo.GetPort() != 0 {
		return &firewall.Port{
			Values: []uint16{uint16(int(portInfo.GetPort()))},
		}
	}

	if portInfo.GetRange() != nil {
		return &firewall.Port{
			IsRange: true,
			Values:  []uint16{uint16(portInfo.GetRange().Start), uint16(portInfo.GetRange().End)},
		}
	}

	return nil
}

func determineDestination(rule *mgmProto.RouteFirewallRule, dynamicResolver bool, sources []netip.Prefix) (firewall.Network, error) {
	var destination firewall.Network

	if rule.IsDynamic {
		if dynamicResolver {
			if len(rule.Domains) > 0 {
				destination.Set = firewall.NewDomainSet(domain.FromPunycodeList(rule.Domains))
			} else {
				// isDynamic is set but no domains = outdated management server
				log.Warn("connected to an older version of management server (no domains in rules), using default destination")
				destination.Prefix = getDefault(sources[0])
			}
		} else {
			// client resolves DNS, we (router) don't know the destination
			destination.Prefix = getDefault(sources[0])
		}
		return destination, nil
	}

	prefix, err := netip.ParsePrefix(rule.Destination)
	if err != nil {
		return destination, fmt.Errorf("parse destination: %w", err)
	}
	destination.Prefix = prefix
	return destination, nil
}

func getDefault(prefix netip.Prefix) netip.Prefix {
	if prefix.Addr().Is6() {
		return netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	}
	return netip.PrefixFrom(netip.IPv4Unspecified(), 0)
}
