// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package node

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/byteorder"
	"github.com/cilium/cilium/pkg/cidr"
	"github.com/cilium/cilium/pkg/common"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	wgTypes "github.com/cilium/cilium/pkg/wireguard/types"
)

const preferPublicIP bool = true

var (
	// addrsMu protects addrs. Outside the addresses struct
	// so that we can Uninitialize() without linter complaining
	// about lock copying.
	addrsMu lock.RWMutex
	addrs   addresses

	// localNode holds the current state of the local "types.Node".
	// This is defined here until all uses of the getters and
	// setters in this file have been migrated to use LocalNodeStore
	// directly.
	// Initialized to proper instance via an invoke function in LocalNodeStoreCell,
	// or temporarily in tests with 'WithTestLocalNodeStore'.
	localNode *LocalNodeStore
)

func getLocalNode() LocalNode {
	n, err := localNode.Get(context.TODO())
	if err != nil {
		// Only expecting errors if we're called after LocalNodeStore has stopped, e.g.
		// we have a component that uses the legacy getters and setters here and does
		// not depend on LocalNodeStore.
		log.WithError(err).Fatal("getLocalNode: unexpected error")
	}
	return n
}

type addresses struct {
	ipv4Loopback      net.IP
	ipv4NodePortAddrs map[string]net.IP // iface name => ip addr
	ipv4MasqAddrs     map[string]net.IP // iface name => ip addr
	ipv6NodePortAddrs map[string]net.IP // iface name => ip addr
	ipv6MasqAddrs     map[string]net.IP // iface name => ip addr
	routerInfo        RouterInfo
}

type RouterInfo interface {
	GetIPv4CIDRs() []net.IPNet
}

func makeIPv6HostIP() net.IP {
	ipstr := "fc00::10CA:1"
	ip := net.ParseIP(ipstr)
	if ip == nil {
		log.WithField(logfields.IPAddr, ipstr).Fatal("Unable to parse IP")
	}

	return ip
}

// InitDefaultPrefix initializes the node address and allocation prefixes with
// default values derived from the system. device can be set to the primary
// network device of the system in which case the first address with global
// scope will be regarded as the system's node address.
func InitDefaultPrefix(device string) {
	localNode.Update(func(n *LocalNode) {
		SetDefaultPrefix(option.Config, device, n)
	})
}

func SetDefaultPrefix(cfg *option.DaemonConfig, device string, node *LocalNode) {
	if cfg.EnableIPv4 {
		isIPv6 := false

		ip, err := firstGlobalV4Addr(device, node.GetCiliumInternalIP(isIPv6), preferPublicIP)
		if err != nil {
			return
		}

		if node.GetNodeIP(isIPv6) == nil {
			node.SetNodeInternalIP(ip)
		}

		ipv4range := node.IPv4AllocCIDR
		ipv6range := node.IPv6AllocCIDR

		if ipv4range == nil {
			// If the IPv6AllocRange is not nil then the IPv4 allocation should be
			// derived from the IPv6AllocRange.
			//                     vvvv vvvv
			// FD00:0000:0000:0000:0000:0000:0000:0000
			if ipv6range != nil {
				ip = net.IPv4(
					ipv6range.IP[8],
					ipv6range.IP[9],
					ipv6range.IP[10],
					ipv6range.IP[11])
			}
			v4range := fmt.Sprintf(defaults.DefaultIPv4Prefix+"/%d",
				ip.To4()[3], defaults.DefaultIPv4PrefixLen)
			_, ip4net, err := net.ParseCIDR(v4range)
			if err != nil {
				log.WithError(err).WithField(logfields.V4Prefix, v4range).Panic("BUG: Invalid default IPv4 prefix")
			}

			node.IPv4AllocCIDR = cidr.NewCIDR(ip4net)
			log.WithField(logfields.V4Prefix, node.IPv4AllocCIDR).Info("Using autogenerated IPv4 allocation range")
		}
	}

	if cfg.EnableIPv6 {
		isIPv6 := true
		ipv4range := node.IPv4AllocCIDR
		ipv6range := node.IPv6AllocCIDR

		if node.GetNodeIP(isIPv6) == nil {
			// Find a IPv6 node address first
			addr, _ := firstGlobalV6Addr(device, node.GetCiliumInternalIP(isIPv6), preferPublicIP)
			if addr == nil {
				addr = makeIPv6HostIP()
			}
			node.SetNodeInternalIP(addr)
		}

		if ipv6range == nil && ipv4range != nil {
			// The IPv6 allocation should be derived from the IPv4 allocation.
			ip := ipv4range.IP
			v6range := fmt.Sprintf("%s%02x%02x:%02x%02x:0:0/%d",
				cfg.IPv6ClusterAllocCIDRBase, ip[0], ip[1], ip[2], ip[3], 96)

			_, ip6net, err := net.ParseCIDR(v6range)
			if err != nil {
				log.WithError(err).WithField(logfields.V6Prefix, v6range).Panic("BUG: Invalid default IPv6 prefix")
			}

			node.IPv6AllocCIDR = cidr.NewCIDR(ip6net)
			log.WithField(logfields.V6Prefix, node.IPv6AllocCIDR).Info("Using autogenerated IPv6 allocation range")
		}
	}
}

// InitNodePortAddrs initializes NodePort IPv{4,6} addrs for the given devices.
// If inheritIPAddrFromDevice is non-empty, then the IP addr for the devices
// will be derived from it.
func InitNodePortAddrs(devices []string, inheritIPAddrFromDevice string) error {
	addrsMu.Lock()
	defer addrsMu.Unlock()

	var inheritedIP net.IP
	var err error

	if option.Config.EnableIPv4 {
		if inheritIPAddrFromDevice != "" {
			inheritedIP, err = firstGlobalV4Addr(inheritIPAddrFromDevice, GetK8sNodeIP(), !preferPublicIP)
			if err != nil {
				return fmt.Errorf("failed to determine IPv4 of %s for NodePort", inheritIPAddrFromDevice)
			}
		}
		addrs.ipv4NodePortAddrs = make(map[string]net.IP, len(devices))
		for _, device := range devices {
			if inheritIPAddrFromDevice != "" {
				addrs.ipv4NodePortAddrs[device] = inheritedIP
			} else {
				ip, err := firstGlobalV4Addr(device, GetK8sNodeIP(), !preferPublicIP)
				if err != nil {
					return fmt.Errorf("failed to determine IPv4 of %s for NodePort", device)
				}
				addrs.ipv4NodePortAddrs[device] = ip
			}
		}
	}

	if option.Config.EnableIPv6 {
		if inheritIPAddrFromDevice != "" {
			inheritedIP, err = firstGlobalV6Addr(inheritIPAddrFromDevice, GetK8sNodeIP(), !preferPublicIP)
			if err != nil {
				return fmt.Errorf("Failed to determine IPv6 of %s for NodePort", inheritIPAddrFromDevice)
			}
		}
		addrs.ipv6NodePortAddrs = make(map[string]net.IP, len(devices))
		for _, device := range devices {
			if inheritIPAddrFromDevice != "" {
				addrs.ipv6NodePortAddrs[device] = inheritedIP
			} else {
				ip, err := firstGlobalV6Addr(device, GetK8sNodeIP(), !preferPublicIP)
				if err != nil {
					return fmt.Errorf("Failed to determine IPv6 of %s for NodePort", device)
				}
				addrs.ipv6NodePortAddrs[device] = ip
			}
		}
	}

	return nil
}

// InitBPFMasqueradeAddrs initializes BPF masquerade addrs for the given devices.
func InitBPFMasqueradeAddrs(devices []string) error {
	addrsMu.Lock()
	defer addrsMu.Unlock()

	masqIPFromDevice := option.Config.DeriveMasqIPAddrFromDevice

	if option.Config.EnableIPv4 {
		addrs.ipv4MasqAddrs = make(map[string]net.IP, len(devices))
		err := initMasqueradeV4Addrs(addrs.ipv4MasqAddrs, masqIPFromDevice, devices, logfields.IPv4)
		if err != nil {
			return err
		}
	}
	if option.Config.EnableIPv6 {
		addrs.ipv6MasqAddrs = make(map[string]net.IP, len(devices))
		return initMasqueradeV6Addrs(addrs.ipv6MasqAddrs, masqIPFromDevice, devices, logfields.IPv6)
	}

	return nil
}

func clone(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

// GetIPv4Loopback returns the loopback IPv4 address of this node.
func GetIPv4Loopback() net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	return clone(addrs.ipv4Loopback)
}

// SetIPv4Loopback sets the loopback IPv4 address of this node.
func SetIPv4Loopback(ip net.IP) {
	addrsMu.Lock()
	addrs.ipv4Loopback = clone(ip)
	addrsMu.Unlock()
}

// GetIPv4AllocRange returns the IPv4 allocation prefix of this node
func GetIPv4AllocRange() *cidr.CIDR {
	return getLocalNode().IPv4AllocCIDR.DeepCopy()
}

// GetIPv6AllocRange returns the IPv6 allocation prefix of this node
func GetIPv6AllocRange() *cidr.CIDR {
	return getLocalNode().IPv6AllocCIDR.DeepCopy()
}

// GetIPv4 returns one of the IPv4 node address available with the following
// priority:
// - NodeInternalIP
// - NodeExternalIP
// - other IP address type.
// It must be reachable on the network.
func GetIPv4() net.IP {
	n := getLocalNode()
	return clone(n.GetNodeIP(false))
}

// GetInternalIPv4 returns node internal ipv4 address else return nil.
func GetInternalIPv4() net.IP {
	n := getLocalNode()
	return clone(n.GetNodeInternalIPv4())
}

// GetInternalIPv6 returns node internal ipv6 address else return nil.
func GetInternalIPv6() net.IP {
	n := getLocalNode()
	return clone(n.GetNodeInternalIPv6())
}

// GetCiliumEndpointNodeIP is the node IP that will be referenced by CiliumEndpoints with endpoints
// running on this node.
func GetCiliumEndpointNodeIP() string {
	if option.Config.EnableIPv4 {
		return GetIPv4().String()
	}
	return GetIPv6().String()
}

// SetInternalIPv4Router sets the cilium internal IPv4 node address, it is allocated from the node prefix.
// This must not be conflated with k8s internal IP as this IP address is only relevant within the
// Cilium-managed network (this means within the node for direct routing mode and on the overlay
// for tunnel mode).
func SetInternalIPv4Router(ip net.IP) {
	localNode.Update(func(n *LocalNode) {
		n.SetCiliumInternalIP(ip)
	})
}

// GetInternalIPv4Router returns the cilium internal IPv4 node address. This must not be conflated with
// k8s internal IP as this IP address is only relevant within the Cilium-managed network (this means
// within the node for direct routing mode and on the overlay for tunnel mode).
func GetInternalIPv4Router() net.IP {
	n := getLocalNode()
	return n.GetCiliumInternalIP(false)
}

// GetK8sExternalIPv4 returns the external IPv4 node address. It must be a public IP that is routable
// on the network as well as the internet. It can return nil if no External IPv4 address is assigned.
func GetK8sExternalIPv4() net.IP {
	n := getLocalNode()
	return n.GetExternalIP(false)
}

// GetRouterInfo returns additional information for the router, the cilium_host interface.
func GetRouterInfo() RouterInfo {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	return addrs.routerInfo
}

// SetRouterInfo sets additional information for the router, the cilium_host interface.
func SetRouterInfo(info RouterInfo) {
	addrsMu.Lock()
	addrs.routerInfo = info
	addrsMu.Unlock()
}

// GetHostMasqueradeIPv4 returns the IPv4 address to be used for masquerading
// any traffic that is being forwarded from the host into the Cilium cluster.
func GetHostMasqueradeIPv4() net.IP {
	return GetInternalIPv4Router()
}

// SetIPv4AllocRange sets the IPv4 address pool to use when allocating
// addresses for local endpoints
func SetIPv4AllocRange(net *cidr.CIDR) {
	localNode.Update(func(n *LocalNode) {
		n.IPv4AllocCIDR = net
	})
}

// GetNodePortIPv4Addrs returns the node-port IPv4 address for NAT
func GetNodePortIPv4Addrs() []net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	addrs4 := make([]net.IP, 0, len(addrs.ipv4NodePortAddrs))
	for _, addr := range addrs.ipv4NodePortAddrs {
		addrs4 = append(addrs4, clone(addr))
	}
	return addrs4
}

// GetNodePortIPv4AddrsWithDevices returns the map iface => NodePort IPv4.
func GetNodePortIPv4AddrsWithDevices() map[string]net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	return copyStringToNetIPMap(addrs.ipv4NodePortAddrs)
}

// GetNodePortIPv6Addrs returns the node-port IPv6 address for NAT
func GetNodePortIPv6Addrs() []net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	addrs6 := make([]net.IP, 0, len(addrs.ipv6NodePortAddrs))
	for _, addr := range addrs.ipv6NodePortAddrs {
		addrs6 = append(addrs6, clone(addr))
	}
	return addrs6
}

// GetNodePortIPv6AddrsWithDevices returns the map iface => NodePort IPv6.
func GetNodePortIPv6AddrsWithDevices() map[string]net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	return copyStringToNetIPMap(addrs.ipv6NodePortAddrs)
}

// GetMasqIPv4AddrsWithDevices returns the map iface => BPF masquerade IPv4.
func GetMasqIPv4AddrsWithDevices() map[string]net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	return copyStringToNetIPMap(addrs.ipv4MasqAddrs)
}

// GetMasqIPv6AddrsWithDevices returns the map iface => BPF masquerade IPv6.
func GetMasqIPv6AddrsWithDevices() map[string]net.IP {
	addrsMu.RLock()
	defer addrsMu.RUnlock()
	return copyStringToNetIPMap(addrs.ipv6MasqAddrs)
}

// SetIPv6NodeRange sets the IPv6 address pool to be used on this node
func SetIPv6NodeRange(net *cidr.CIDR) {
	localNode.Update(func(n *LocalNode) {
		n.IPv6AllocCIDR = net
	})
}

// AutoComplete completes the parts of addressing that can be auto derived
func AutoComplete() error {
	InitDefaultPrefix(option.Config.DirectRoutingDevice)

	if option.Config.EnableIPv6 && GetIPv6AllocRange() == nil {
		return fmt.Errorf("IPv6 allocation CIDR is not configured. Please specify --%s", option.IPv6Range)
	}

	if option.Config.EnableIPv4 && GetIPv4AllocRange() == nil {
		return fmt.Errorf("IPv4 allocation CIDR is not configured. Please specify --%s", option.IPv4Range)
	}

	return nil
}

// ValidatePostInit validates the entire addressing setup and completes it as
// required
func ValidatePostInit() error {
	if option.Config.EnableIPv4 || option.Config.TunnelingEnabled() {
		if GetIPv4() == nil {
			return fmt.Errorf("external IPv4 node address could not be derived, please configure via --ipv4-node")
		}
	}

	if option.Config.EnableIPv4 && GetInternalIPv4Router() == nil {
		return fmt.Errorf("BUG: Internal IPv4 node address was not configured")
	}

	return nil
}

// GetIPv6 returns the IPv6 address of the node
func GetIPv6() net.IP {
	n := getLocalNode()
	return clone(n.GetNodeIP(true))
}

// GetHostMasqueradeIPv6 returns the IPv6 address to be used for masquerading
// any traffic that is being forwarded from the host into the Cilium cluster.
func GetHostMasqueradeIPv6() net.IP {
	return GetIPv6Router()
}

// GetIPv6Router returns the IPv6 address of the router, e.g. address
// of cilium_host device.
func GetIPv6Router() net.IP {
	n := getLocalNode()
	return clone(n.GetCiliumInternalIP(true))
}

// SetIPv6Router sets the IPv6 address of the router address, e.g. address
// of cilium_host device.
func SetIPv6Router(ip net.IP) {
	localNode.Update(func(n *LocalNode) {
		n.SetCiliumInternalIP(ip)
	})
}

// GetK8sExternalIPv6 returns the external IPv6 node address.
func GetK8sExternalIPv6() net.IP {
	n := getLocalNode()
	return clone(n.GetExternalIP(false))
}

// GetNodeAddressing returns the NodeAddressing model for the local IPs.
func GetNodeAddressing() *models.NodeAddressing {
	a := &models.NodeAddressing{}

	if option.Config.EnableIPv6 {
		a.IPV6 = &models.NodeAddressingElement{
			Enabled:    option.Config.EnableIPv6,
			IP:         GetIPv6Router().String(),
			AllocRange: GetIPv6AllocRange().String(),
		}
	}

	if option.Config.EnableIPv4 {
		a.IPV4 = &models.NodeAddressingElement{
			Enabled:    option.Config.EnableIPv4,
			IP:         GetInternalIPv4Router().String(),
			AllocRange: GetIPv4AllocRange().String(),
		}
	}

	return a
}

func getCiliumHostIPsFromFile(nodeConfig string) (ipv4GW, ipv6Router net.IP) {
	// ipLen is the length of the IP address stored in the node_config.h
	// it has the same length for both IPv4 and IPv6.
	const ipLen = net.IPv6len

	var hasIPv4, hasIPv6 bool
	f, err := os.Open(nodeConfig)
	switch {
	case err != nil:
	default:
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			txt := scanner.Text()
			switch {
			case !hasIPv6 && strings.Contains(txt, defaults.RestoreV6Addr):
				defineLine := strings.Split(txt, defaults.RestoreV6Addr)
				if len(defineLine) != 2 {
					continue
				}
				ipv6 := common.C2GoArray(defineLine[1])
				if len(ipv6) != ipLen {
					continue
				}
				ipv6Router = net.IP(ipv6)
				hasIPv6 = true
			case !hasIPv4 && strings.Contains(txt, defaults.RestoreV4Addr):
				defineLine := strings.Split(txt, defaults.RestoreV4Addr)
				if len(defineLine) != 2 {
					continue
				}
				ipv4 := common.C2GoArray(defineLine[1])
				if len(ipv4) != ipLen {
					continue
				}
				ipv4GW = net.IP(ipv4)
				hasIPv4 = true

			// Legacy cases based on the header defines:
			case !hasIPv4 && strings.Contains(txt, "IPV4_GATEWAY"):
				// #define IPV4_GATEWAY 0xee1c000a
				defineLine := strings.Split(txt, " ")
				if len(defineLine) != 3 {
					continue
				}
				ipv4GWHex := strings.TrimPrefix(defineLine[2], "0x")
				ipv4GWUint64, err := strconv.ParseUint(ipv4GWHex, 16, 32)
				if err != nil {
					continue
				}
				if ipv4GWUint64 != 0 {
					bs := make([]byte, net.IPv4len)
					byteorder.Native.PutUint32(bs, uint32(ipv4GWUint64))
					ipv4GW = net.IPv4(bs[0], bs[1], bs[2], bs[3])
					hasIPv4 = true
				}
			case !hasIPv6 && strings.Contains(txt, " ROUTER_IP "):
				// #define ROUTER_IP 0xf0, 0xd, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xa, 0x0, 0x0, 0x0, 0x0, 0x0, 0x8a, 0xd6
				defineLine := strings.Split(txt, " ROUTER_IP ")
				if len(defineLine) != 2 {
					continue
				}
				ipv6 := common.C2GoArray(defineLine[1])
				if len(ipv6) != net.IPv6len {
					continue
				}
				ipv6Router = net.IP(ipv6)
				hasIPv6 = true
			}
		}
	}
	return ipv4GW, ipv6Router
}

// ExtractCiliumHostIPFromFS returns the Cilium IPv4 gateway and router IPv6 address from
// the node_config.h file if is present; or by deriving it from
// defaults.HostDevice interface, on which only the IPv4 is possible to derive.
func ExtractCiliumHostIPFromFS() (ipv4GW, ipv6Router net.IP) {
	if !option.Config.EnableHostIPRestore {
		return nil, nil
	}

	nodeConfig := option.Config.GetNodeConfigPath()
	ipv4GW, ipv6Router = getCiliumHostIPsFromFile(nodeConfig)
	if ipv4GW != nil || ipv6Router != nil {
		log.WithFields(logrus.Fields{
			"ipv4": ipv4GW,
			"ipv6": ipv6Router,
			"file": nodeConfig,
		}).Info("Restored router address from node_config")
		return ipv4GW, ipv6Router
	}
	return getCiliumHostIPsFromNetDev(defaults.HostDevice)
}

// SetIPsecKeyIdentity sets the IPsec key identity an opaque value used to
// identity encryption keys used on the node.
func SetIPsecKeyIdentity(id uint8) {
	localNode.Update(func(n *LocalNode) {
		n.EncryptionKey = id
	})
}

// GetIPsecKeyIdentity returns the IPsec key identity of the node
func GetIPsecKeyIdentity() uint8 {
	return getLocalNode().EncryptionKey
}

// GetK8sNodeIPs returns k8s Node IP addr.
func GetK8sNodeIP() net.IP {
	n := getLocalNode()
	return n.GetK8sNodeIP()
}

func GetWireguardPubKey() string {
	return getLocalNode().WireguardPubKey
}

func GetOptOutNodeEncryption() bool {
	return getLocalNode().OptOutNodeEncryption
}

func SetOptOutNodeEncryption(b bool) {
	localNode.Update(func(node *LocalNode) {
		node.OptOutNodeEncryption = b
	})
}

// SetEndpointHealthIPv4 sets the IPv4 cilium-health endpoint address.
func SetEndpointHealthIPv4(ip net.IP) {
	localNode.Update(func(n *LocalNode) {
		n.IPv4HealthIP = ip
	})
}

// GetEndpointHealthIPv4 returns the IPv4 cilium-health endpoint address.
func GetEndpointHealthIPv4() net.IP {
	return getLocalNode().IPv4HealthIP
}

// SetEndpointHealthIPv6 sets the IPv6 cilium-health endpoint address.
func SetEndpointHealthIPv6(ip net.IP) {
	localNode.Update(func(n *LocalNode) {
		n.IPv6HealthIP = ip
	})
}

// GetEndpointHealthIPv6 returns the IPv6 cilium-health endpoint address.
func GetEndpointHealthIPv6() net.IP {
	return getLocalNode().IPv6HealthIP
}

// SetIngressIPv4 sets the local IPv4 source address for Cilium Ingress.
func SetIngressIPv4(ip net.IP) {
	localNode.Update(func(n *LocalNode) {
		n.IPv4IngressIP = ip
	})
}

// GetIngressIPv4 returns the local IPv4 source address for Cilium Ingress.
func GetIngressIPv4() net.IP {
	return getLocalNode().IPv4IngressIP
}

// SetIngressIPv6 sets the local IPv6 source address for Cilium Ingress.
func SetIngressIPv6(ip net.IP) {
	localNode.Update(func(n *LocalNode) {
		n.IPv6IngressIP = ip
	})
}

// GetIngressIPv6 returns the local IPv6 source address for Cilium Ingress.
func GetIngressIPv6() net.IP {
	return getLocalNode().IPv6IngressIP
}

// GetEncryptKeyIndex returns the encryption key value for the local node.
// With IPSec encryption, this is equivalent to GetIPsecKeyIdentity().
// With WireGuard encryption, this function returns a non-zero static value
// if the local node has WireGuard enabled.
func GetEncryptKeyIndex() uint8 {
	switch {
	case option.Config.EnableIPSec:
		return GetIPsecKeyIdentity()
	case option.Config.EnableWireguard:
		if len(GetWireguardPubKey()) > 0 {
			return wgTypes.StaticEncryptKey
		}
	}
	return 0
}

func copyStringToNetIPMap(in map[string]net.IP) map[string]net.IP {
	out := make(map[string]net.IP, len(in))
	for iface, ip := range in {
		dup := make(net.IP, len(ip))
		copy(dup, ip)
		out[iface] = dup
	}
	return out
}

// WithTestLocalNodeStore sets the 'localNode' to a temporary instance and
// runs the given test. Afterwards the 'localNode' is restored to nil.
// This is a temporary workaround for tests until the LocalNodeStoreCell can be
// used.
func WithTestLocalNodeStore(runTest func()) {
	SetTestLocalNodeStore()
	defer UnsetTestLocalNodeStore()
	runTest()
}

func SetTestLocalNodeStore() {
	if localNode != nil {
		panic("localNode already set")
	}

	// Set the localNode global variable temporarily so that the legacy getters
	// and setters can access it.
	localNode = NewTestLocalNodeStore(LocalNode{})
}

func UnsetTestLocalNodeStore() {
	localNode = nil
}

// UpdateLocalNodeInTest provides access to modifying the local node
// information from tests that are not yet using hive and the LocalNodeStoreCell.
func UpdateLocalNodeInTest(mod func(n *LocalNode)) {
	if localNode == nil {
		panic("localNode not set, use node.LocalNodeStoreCell or WithTestLocalNodeStore()?")
	}
	localNode.Update(mod)
}
