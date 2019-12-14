package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
	dockertypes "github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
	"github.com/j-keck/arping"
	"github.com/vishvananda/netlink"
)

const (
	CheckPointFile                      = "/home/work/cni-plugins/checkpoints/%s"
	ipv4InterfaceArpProxySysctlTemplate = "net.ipv4.conf.%s.proxy_arp"
)

var (
	check bool
	fix   bool
)

type (
	CheckPoint struct {
		Netns   string          `json:"netns"`
		Podname string          `json:"podname"`
		Sandbox string          `json:"sandbox"`
		Ifname  string          `json:"ifname"`
		Result  *current.Result `json:"result"`
	}

	netConf struct {
		types.NetConf
		Master   string `json:"master"`
		Master2  string `json:"master2"`
		VlanID   int    `json:"vlanID"`
		Mode     string `json:"mode"`
		MTU      int    `json:"mtu"`
		IPAM     string `json:"ipamServer"`
		Token    string `json:"ipamToken"`
		LogDir   string `json:"logDir"`
		CheckDir string `json:"checkDir"`
	}
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
	flag.BoolVar(&check, "check", true, "执行检查运行的容器对应的checkpoint是否存在")
	flag.BoolVar(&fix, "fix", false, "是否执行修复")
}

func main() {
	flag.Parse()
	endpoint := "unix:///var/run/docker.sock"
	client, err := dockerclient.NewClient(endpoint, "v1.13.1", nil, nil)
	if err != nil {
		panic(err)
	}

	containers, err := client.ContainerList(context.TODO(), dockertypes.ContainerListOptions{})
	if err != nil {
		panic(err)
	}

	ids := make([]string, 0, len(containers))
	errMsg := make([]string, 0, len(containers))
	for _, c := range containers {
		if c.NetworkSettings != nil && len(c.NetworkSettings.Networks) != 0 {
			id := c.ID[:12]
			ids = append(ids, id)
			file := fmt.Sprintf(CheckPointFile, id)
			if _, err := os.Stat(file); os.IsNotExist(err) {
				errMsg = append(errMsg, id)
			}

		}
	}
	if check && len(errMsg) != 0 {
		fmt.Printf("%s \ncheckpoint file not exist, check it manually\n", strings.Join(errMsg, "\n"))
		return
	} else {
		fmt.Println("checkpoint files of containers are ok")
	}

	if !fix {
		return
	}

	for _, id := range ids {
		file := fmt.Sprintf(CheckPointFile, id)
		data, err := ioutil.ReadFile(file)
		checkpoint := CheckPoint{}
		err = json.Unmarshal(data, &checkpoint)
		if err != nil {
			log.Printf("file %s unmarshal err:%s", file, err)
			continue
		}
		cfg := &netConf{
			NetConf: types.NetConf{},
			Master:  "bond4",
			Master2: "eth0",
			VlanID:  3,
			MTU:     1500,
		}

		netns, err := ns.GetNS(checkpoint.Netns)
		if err != nil {
			log.Printf("failed to open netns %q: %v", netns, err)
			continue
		}
		defer netns.Close()

		result := checkpoint.Result
		macvlanInterface, err := createMacvlan(cfg, checkpoint.Ifname, netns, true, checkpoint.Result.Interfaces[0].Mac)
		if err != nil {
			log.Printf("create macvlanInterface for container %s, checkpoint file %s, err %s\n", id, file, err)
			continue
		} else {
			log.Printf("create macvlanInterface success! id %s file %s => name %s  mac %s(%s) sandbox %s \n", id, file,
				macvlanInterface.Name, macvlanInterface.Mac, checkpoint.Result.Interfaces[0].Mac, macvlanInterface.Sandbox)
		}

		result.Interfaces = []*current.Interface{macvlanInterface}

		for _, ipc := range result.IPs {
			// All addresses apply to the container macvlan interface
			ipc.Interface = current.Int(0)
		}

		err = netns.Do(func(_ ns.NetNS) error {
			if err := ipam.ConfigureIface(checkpoint.Ifname, result); err != nil {
				return err
			}

			contVeth, err := net.InterfaceByName(checkpoint.Ifname)
			if err != nil {
				return fmt.Errorf("failed to look up %q: %v", checkpoint.Ifname, err)
			}

			for _, ipc := range result.IPs {
				if ipc.Version == "4" {
					_ = arping.GratuitousArpOverIface(ipc.Address.IP, *contVeth)
				}
			}
			return nil
		})
		if err != nil {
			log.Println(err)
			continue
		}
	}
}

func ensureVlan(master string, master2 string, vlanID int) (netlink.Link, error) {
	m, err := netlink.LinkByName(master) // first try bond4, if not found, use eth0
	if err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
		master = master2
	}

	vlanLinkName := fmt.Sprintf("%s.%d", master, vlanID)
	vlanLink, err := netlink.LinkByName(vlanLinkName)
	if err != nil {
		if !strings.Contains(err.Error(), "not found") {
			log.Printf("failed to lookup vlan %s: %v", vlanLinkName, err)
			return nil, err
		}
	}
	if vlanLink != nil {
		return vlanLink, nil
	}

	m, err = netlink.LinkByName(master)
	if err != nil {
		log.Printf("failed to lookup master %s: %v", master, err)
		return nil, err
	}
	vlan := &netlink.Vlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        vlanLinkName,
			ParentIndex: m.Attrs().Index,
		},
		VlanId: vlanID,
	}
	if err := netlink.LinkAdd(vlan); err != nil {
		log.Printf("failed to add vlan %s: %v", vlanLinkName, err)
		return nil, err
	}
	result, err := netlink.LinkByName(vlanLinkName)
	if err != nil {
		log.Printf("failed to lookup vlan(2) %s: %v", vlanLinkName, err)
		return nil, err
	}

	if err := netlink.LinkSetUp(result); err != nil {
		log.Printf("failed to set up vlan link %s: %v", vlanLinkName, err)
		return nil, err
	}
	return result, nil
}

func createMacvlan(conf *netConf, ifName string, netns ns.NetNS, useVlan bool, mac string) (*current.Interface, error) {
	macvlan := &current.Interface{}
	var m netlink.Link
	var err error
	if useVlan {
		m, err = ensureVlan(conf.Master, conf.Master2, conf.VlanID)
		if err != nil {
			return nil, err
		}
	} else {
		m, err = netlink.LinkByName(conf.Master) // first try bond4, if not found, use eth0
		if err != nil {
			if !strings.Contains(err.Error(), "not found") {
				return nil, err
			}
			m, err = netlink.LinkByName(conf.Master2)
			if err != nil {
				return nil, err
			}
		}
	}

	// due to kernel bug we have to create with tmpName or it might
	// collide with the name on the host and error out
	tmpName, err := ip.RandomVethName()
	if err != nil {
		return nil, err
	}

	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			MTU:         conf.MTU,
			Name:        tmpName,
			ParentIndex: m.Attrs().Index,
			Namespace:   netlink.NsFd(int(netns.Fd())),
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}

	if err := netlink.LinkAdd(mv); err != nil {
		return nil, fmt.Errorf("failed to create macvlan: %v", err)
	}

	err = netns.Do(func(_ ns.NetNS) error {
		// TODO: duplicate following lines for ipv6 support, when it will be added in other places
		ipv4SysctlValueName := fmt.Sprintf(ipv4InterfaceArpProxySysctlTemplate, tmpName)
		if _, err := sysctl.Sysctl(ipv4SysctlValueName, "1"); err != nil {
			// remove the newly added link and ignore errors, because we already are in a failed state
			_ = netlink.LinkDel(mv)
			return fmt.Errorf("failed to set proxy_arp on newly added interface %q: %v", tmpName, err)
		}

		err := ip.RenameLink(tmpName, ifName)
		if err != nil {
			_ = netlink.LinkDel(mv)
			return fmt.Errorf("failed to rename macvlan to %q: %v", ifName, err)
		}
		macvlan.Name = ifName

		// Re-fetch macvlan to get all properties/attributes
		contMacvlan, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to refetch macvlan %q: %v", ifName, err)
		}
		//directly use the mac from sdn
		if len(mac) != 0 {
			hardwareAddr, err := net.ParseMAC(mac)
			if err != nil {
				return err
			}
			if err := netlink.LinkSetHardwareAddr(contMacvlan, hardwareAddr); err != nil {
				return fmt.Errorf("failed to change macvlan mac: %v", err)
			}
			contMacvlan, err = netlink.LinkByName(ifName)
			if err != nil {
				return fmt.Errorf("failed to refetch macvlan %q: %v", ifName, err)
			}
		}
		macvlan.Mac = contMacvlan.Attrs().HardwareAddr.String()
		macvlan.Sandbox = netns.Path()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return macvlan, nil
}
