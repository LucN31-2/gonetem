package server

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/mroy31/gonetem/internal/link"
	"github.com/mroy31/gonetem/internal/options"
	"github.com/mroy31/gonetem/internal/ovs"
	"github.com/mroy31/gonetem/internal/proto"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

const (
	networkFilename = "network.yml"
	configDir       = "configs"
)

var (
	mutex = &sync.Mutex{}
)

type VrrpOptions struct {
	Interface int
	Group     int
	Address   string
}

type NodeConfig struct {
	Type    string
	IPv6    bool
	Mpls    bool
	Vrfs    []string
	Vrrps   []VrrpOptions
	Volumes []string
	Image   string
}

type LinkConfig struct {
	Peer1  string
	Peer2  string
	Loss   float64 // percent
	Delay  int     // ms
	Jitter int     // ms
	Rate   int     // kbps
}

type BridgeConfig struct {
	Host       string
	Interfaces []string
}

type NetemTopology struct {
	Nodes   map[string]NodeConfig
	Links   []LinkConfig
	Bridges map[string]BridgeConfig
}

type NetemLinkPeer struct {
	Node    INetemNode
	IfIndex int
}

type NetemLink struct {
	Peer1  NetemLinkPeer
	Peer2  NetemLinkPeer
	Loss   float64
	Delay  int
	Jitter int
	Rate   int
}

type NetemBridge struct {
	Name          string
	HostInterface string
	Peers         []NetemLinkPeer
}

type NetemTopologyManager struct {
	prjID string
	path  string

	IdGenerator *NodeIdentifierGenerator
	nodes       []INetemNode
	ovsInstance *ovs.OvsProjectInstance
	links       []*NetemLink
	bridges     []*NetemBridge
	running     bool
	logger      *logrus.Entry
}

func (t *NetemTopologyManager) Check() error {
	filepath := path.Join(t.path, networkFilename)
	_, errors := CheckTopology(filepath)
	if len(errors) > 0 {
		msg := ""
		for _, err := range errors {
			msg += "\n\t" + err.Error()
		}
		return fmt.Errorf("Topology is not valid:%s\n", msg)
	}

	return nil
}

func (t *NetemTopologyManager) Load() error {
	filepath := path.Join(t.path, networkFilename)
	topology, errors := CheckTopology(filepath)
	if len(errors) > 0 {
		msg := ""
		for _, err := range errors {
			msg += "\n\t" + err.Error()
		}
		return fmt.Errorf("Topology if not valid:%s\n", msg)
	}

	var err error
	// Create openvswitch instance for this project
	t.ovsInstance, err = ovs.NewOvsInstance(t.prjID)
	if err != nil {
		return err
	}

	// Create nodes
	t.nodes = make([]INetemNode, 0)
	g := new(errgroup.Group)

	for name, nConfig := range topology.Nodes {
		name := name
		nConfig := nConfig

		g.Go(func() error {
			t.logger.Debugf("Create node %s", name)

			shortName, err := t.IdGenerator.GetId(name)
			if err != nil {
				return err
			}
			node, err := CreateNode(t.prjID, name, shortName, nConfig)

			mutex.Lock()
			t.nodes = append(t.nodes, node)
			mutex.Unlock()

			if err != nil {
				return fmt.Errorf("Unable to create node %s: %w", name, err)
			}

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Create links
	t.links = make([]*NetemLink, len(topology.Links))
	for idx, lConfig := range topology.Links {
		peer1 := strings.Split(lConfig.Peer1, ".")
		peer2 := strings.Split(lConfig.Peer2, ".")

		peer1Idx, _ := strconv.Atoi(peer1[1])
		peer2Idx, _ := strconv.Atoi(peer2[1])

		t.links[idx] = &NetemLink{
			Peer1: NetemLinkPeer{
				Node:    t.GetNode(peer1[0]),
				IfIndex: peer1Idx,
			},
			Peer2: NetemLinkPeer{
				Node:    t.GetNode(peer2[0]),
				IfIndex: peer2Idx,
			},
			Delay:  lConfig.Delay,
			Jitter: lConfig.Jitter,
			Loss:   lConfig.Loss,
			Rate:   lConfig.Rate,
		}
	}

	// Create bridges
	bIdx := 0
	t.bridges = make([]*NetemBridge, len(topology.Bridges))
	for bName, bConfig := range topology.Bridges {
		shortName, err := t.IdGenerator.GetId(bName)
		if err != nil {
			return err
		}

		t.bridges[bIdx] = &NetemBridge{
			Name:          options.NETEM_ID + t.prjID + "." + shortName,
			HostInterface: bConfig.Host,
			Peers:         make([]NetemLinkPeer, len(bConfig.Interfaces)),
		}

		for pIdx, ifName := range bConfig.Interfaces {
			peer := strings.Split(ifName, ".")
			peerIdx, _ := strconv.Atoi(peer[1])

			t.bridges[bIdx].Peers[pIdx] = NetemLinkPeer{
				Node:    t.GetNode(peer[0]),
				IfIndex: peerIdx,
			}
		}

		bIdx++
	}

	return nil
}

func (t *NetemTopologyManager) Reload() ([]*proto.RunResponse_NodeMessages, error) {
	var err error
	var nodeMessages []*proto.RunResponse_NodeMessages

	if err = t.Close(); err != nil {
		return nodeMessages, err
	}

	if err = t.Load(); err != nil {
		return nodeMessages, err
	}
	if t.running {
		t.running = false
		return t.Run()
	}

	return nodeMessages, nil
}

func (t *NetemTopologyManager) Run() ([]*proto.RunResponse_NodeMessages, error) {
	t.logger.Debug("Topo/Run")

	var err error
	var nodeMessages []*proto.RunResponse_NodeMessages

	if t.running {
		t.logger.Warn("Topology is already running")
		return nodeMessages, nil
	}

	g := new(errgroup.Group)
	// 1 - start ovswitch container and init p2pSwitch
	t.logger.Debug("Topo/Run: start ovswitch instance")
	t.ovsInstance.Start()
	if err != nil {
		return nodeMessages, err
	}

	// 2 - start all nodes
	t.logger.Debug("Topo/Run: start all nodes")
	for _, node := range t.nodes {
		node := node
		g.Go(func() error { return node.Start() })
	}
	if err := g.Wait(); err != nil {
		return nodeMessages, err
	}

	// 3 - create links
	t.logger.Debug("Topo/Run: setup links")
	for _, l := range t.links {
		if err := t.setupLink(l); err != nil {
			return nodeMessages, err
		}
	}

	// 4 - create bridges
	t.logger.Debug("Topo/Run: setup bridges")
	for _, br := range t.bridges {
		br := br
		g.Go(func() error {
			return t.setupBridge(br)
		})
	}
	if err := g.Wait(); err != nil {
		return nodeMessages, err
	}

	// 5 - load configs
	t.logger.Debug("Topo/Run: load configuration")
	configPath := path.Join(t.path, configDir)
	for _, node := range t.nodes {
		node := node
		g.Go(func() error {
			messages, err := node.LoadConfig(configPath)
			nodeMessages = append(nodeMessages, &proto.RunResponse_NodeMessages{
				Name:     node.GetName(),
				Messages: messages,
			})
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nodeMessages, err
	}

	t.running = true
	return nodeMessages, nil
}

func (t *NetemTopologyManager) setupBridge(br *NetemBridge) error {
	rootNs := link.GetRootNetns()
	defer rootNs.Close()

	brId, err := link.CreateBridge(br.Name, rootNs)
	if err != nil {
		return err
	}

	if err := link.AttachToBridge(brId, br.HostInterface, rootNs); err != nil {
		return fmt.Errorf("Unable to attach HostIf to bridge %s: %v", br.Name, err)
	}

	for _, peer := range br.Peers {
		peerNetns, err := peer.Node.GetNetns()
		if err != nil {
			return err
		}
		defer peerNetns.Close()

		ifName := fmt.Sprintf("%s%s%s.%d", options.NETEM_ID, t.prjID, peer.Node.GetShortName(), peer.IfIndex)
		peerIfName := fmt.Sprintf("%s%s%d.%s", options.NETEM_ID, t.prjID, peer.IfIndex, peer.Node.GetShortName())
		veth, err := link.CreateVethLink(
			ifName, rootNs,
			peerIfName, peerNetns,
		)
		if err != nil {
			return fmt.Errorf(
				"Unable to create link %s-%s.%d: %v",
				br.Name, peer.Node.GetName(), peer.IfIndex, err,
			)
		}

		// set interface up
		if err := link.SetInterfaceState(veth.Name, rootNs, link.IFSTATE_UP); err != nil {
			return err
		}

		if err := link.AttachToBridge(brId, veth.Name, rootNs); err != nil {
			return err
		}
		peer.Node.AddInterface(peerIfName, peer.IfIndex, peerNetns)
	}

	return nil
}

func (t *NetemTopologyManager) setupLink(l *NetemLink) error {
	peer1Netns, err := l.Peer1.Node.GetNetns()
	if err != nil {
		return err
	}
	defer peer1Netns.Close()

	peer2Netns, err := l.Peer2.Node.GetNetns()
	if err != nil {
		return err
	}
	defer peer2Netns.Close()

	peer1IfName := fmt.Sprintf("%s%s.%d", t.prjID, l.Peer1.Node.GetShortName(), l.Peer1.IfIndex)
	peer2IfName := fmt.Sprintf("%s%s.%d", t.prjID, l.Peer2.Node.GetShortName(), l.Peer2.IfIndex)
	_, err = link.CreateVethLink(peer1IfName, peer1Netns, peer2IfName, peer2Netns)
	if err != nil {
		return fmt.Errorf(
			"Unable to create link %s.%d-%s.%d: %v",
			l.Peer1.Node.GetName(), l.Peer1.IfIndex,
			l.Peer2.Node.GetName(), l.Peer2.IfIndex,
			err,
		)
	}

	// create netem qdisc if necessary
	if l.Delay > 0 || l.Loss > 0 {
		if err := link.CreateNetem(peer1IfName, peer1Netns, l.Delay, l.Jitter, l.Loss); err != nil {
			return err
		}
		if err := link.CreateNetem(peer2IfName, peer2Netns, l.Delay, l.Jitter, l.Loss); err != nil {
			return err
		}
	}
	// create tbf qdisc if necessary
	if l.Rate > 0 {
		if err := link.CreateTbf(peer1IfName, peer1Netns, l.Delay+l.Jitter, l.Rate); err != nil {
			return err
		}
		if err := link.CreateTbf(peer2IfName, peer2Netns, l.Delay+l.Jitter, l.Rate); err != nil {
			return err
		}
	}

	if err := l.Peer1.Node.AddInterface(peer1IfName, l.Peer1.IfIndex, peer1Netns); err != nil {
		return err
	}
	if err := l.Peer2.Node.AddInterface(peer2IfName, l.Peer2.IfIndex, peer2Netns); err != nil {
		return err
	}

	return nil
}

func (t *NetemTopologyManager) IsRunning() bool {
	return t.running
}

func (t *NetemTopologyManager) GetNetFilePath() string {
	return path.Join(t.path, networkFilename)
}

func (t *NetemTopologyManager) ReadNetworkFile() ([]byte, error) {
	return ioutil.ReadFile(t.GetNetFilePath())
}

func (t *NetemTopologyManager) WriteNetworkFile(data []byte) error {
	return ioutil.WriteFile(t.GetNetFilePath(), data, 0644)
}

func (t *NetemTopologyManager) GetAllNodes() []INetemNode {
	return t.nodes
}

func (t *NetemTopologyManager) GetNode(name string) INetemNode {
	for _, node := range t.nodes {
		if node.GetName() == name {
			return node
		}
	}
	return nil
}

func (t *NetemTopologyManager) startNode(node INetemNode) ([]string, error) {
	if err := node.Start(); err != nil {
		return []string{}, fmt.Errorf("Unable to start node %s: %w", node.GetName(), err)
	}

	configPath := path.Join(t.path, configDir)
	messages, err := node.LoadConfig(configPath)
	if err != nil {
		return messages, fmt.Errorf("Unable to load config of node %s: %w", node.GetName(), err)
	}

	return messages, nil
}

func (t *NetemTopologyManager) stopNode(node INetemNode) error {
	if err := node.Stop(); err != nil {
		return fmt.Errorf("Unable to stop node %s: %w", node.GetName(), err)
	}
	return nil
}

func (t *NetemTopologyManager) Start(nodeName string) ([]string, error) {
	if !t.running {
		t.logger.Warnf("Start %s: topology not running", nodeName)
		return []string{}, nil
	}

	node := t.GetNode(nodeName)
	if node == nil {
		return []string{}, fmt.Errorf("Node %s not found in the topology", nodeName)
	}

	return t.startNode(node)
}

func (t *NetemTopologyManager) Stop(nodeName string) error {
	if !t.running {
		t.logger.Warnf("Stop %s: topology not running", nodeName)
		return nil
	}

	node := t.GetNode(nodeName)
	if node == nil {
		return fmt.Errorf("Node %s not found in the topology", nodeName)
	}

	return t.stopNode(node)
}

func (t *NetemTopologyManager) Save() error {
	// create config folder if not exist
	destPath := path.Join(t.path, configDir)
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		if err := os.Mkdir(destPath, 0755); err != nil {
			return fmt.Errorf("Unable to create configs dir %s: %w", destPath, err)
		}
	}

	g := new(errgroup.Group)
	for _, node := range t.nodes {
		node := node
		g.Go(func() error { return node.Save(destPath) })
	}
	return g.Wait()
}

func (t *NetemTopologyManager) Close() error {
	g := new(errgroup.Group)
	// close all nodes
	for _, node := range t.nodes {
		node := node
		g.Go(func() error { return node.Close() })
	}
	if err := g.Wait(); err != nil {
		t.logger.Errorf("Error when closing nodes: %v", err)
	}

	rootNs := link.GetRootNetns()
	defer rootNs.Close()
	for _, br := range t.bridges {
		if err := link.DeleteLink(br.Name, rootNs); err != nil {
			t.logger.Warnf("Error when deleting bridge %s: %v", br.Name, err)
		}

		for _, peer := range br.Peers {
			ifName := fmt.Sprintf(
				"%s%s%s.%d", options.NETEM_ID, t.prjID,
				peer.Node.GetShortName(), peer.IfIndex)
			if err := link.DeleteLink(ifName, rootNs); err != nil {
				t.logger.Warnf("Error when deleting link %s: %v", ifName, err)
			}
		}
	}

	t.nodes = make([]INetemNode, 0)
	t.links = make([]*NetemLink, 0)
	t.bridges = make([]*NetemBridge, 0)
	t.IdGenerator.Close()

	if err := ovs.CloseOvsInstance(t.prjID); err != nil {
		t.logger.Warnf("Error when closing ovswitch instance: %v", err)
	}
	t.ovsInstance = nil

	return nil
}

func LoadTopology(prjID, prjPath string) (*NetemTopologyManager, error) {
	topo := &NetemTopologyManager{
		prjID:  prjID,
		path:   prjPath,
		nodes:  make([]INetemNode, 0),
		logger: logrus.WithField("project", prjID),
		IdGenerator: &NodeIdentifierGenerator{
			lock: &sync.Mutex{},
		},
	}
	if err := topo.Load(); err != nil {
		return topo, fmt.Errorf("Unable to load the topology:\n\t%w", err)
	}
	return topo, nil
}
