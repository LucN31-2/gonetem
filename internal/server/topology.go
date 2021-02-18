package server

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/docker/docker/pkg/system"
	"github.com/mroy31/gonetem/internal/docker"
	"github.com/mroy31/gonetem/internal/link"
	"github.com/mroy31/gonetem/internal/ovs"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func shortName(name string) string {
	if len(name) <= 2 {
		return name
	}

	return name[len(name)-3 : len(name)-1]
}

func genIfName(prjId, hostName string, hostIfIndex int, peerName string, peerIfIndex int) string {
	return fmt.Sprintf(
		"%s%s%d.%s%d", prjId,
		shortName(hostName), hostIfIndex,
		shortName(peerName), peerIfIndex)
}

func splitCopyArg(arg string) (container, path string) {
	if system.IsAbs(arg) {
		return "", arg
	}

	parts := strings.SplitN(arg, ":", 2)

	if len(parts) == 1 || strings.HasPrefix(parts[0], ".") {
		// Either there's no `:` in the arg
		// OR it's an explicit local relative path like `./file:name.txt`.
		return "", arg
	}

	return parts[0], parts[1]
}

type copyDirection int

const (
	fromContainer copyDirection = 1 << iota
	toContainer
	acrossContainers = fromContainer | toContainer
)

const (
	networkFilename = "network.yml"
	configDir       = "configs"
)

type NodeConfig struct {
	Name       string
	Type       string
	Interfaces map[string]string
	IPv6       bool
	Mpls       bool
}

type LinkConfig struct {
	Peer1 string
	Peer2 string
}

type NetemTopology struct {
	Nodes []NodeConfig
	Links []LinkConfig
}

type NetemLinkPeer struct {
	Node    INetemNode
	IfIndex int
}

type NetemLink struct {
	Peer1 NetemLinkPeer
	Peer2 NetemLinkPeer
}

type NetemTopologyManager struct {
	prjID string
	path  string

	nodes       []INetemNode
	ovsInstance *ovs.OvsProjectInstance
	links       []*NetemLink
	running     bool
	logger      *logrus.Entry
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
	t.nodes = make([]INetemNode, len(topology.Nodes))
	for idx, nConfig := range topology.Nodes {
		t.logger.Debugf("Create node %s", nConfig.Name)

		t.nodes[idx], err = CreateNode(t.prjID, nConfig)
		if err != nil {
			return fmt.Errorf("Unable to create node %s: %w", nConfig.Name, err)
		}
	}

	// Create links
	for _, lConfig := range topology.Links {
		peer1 := strings.Split(lConfig.Peer1, ".")
		peer2 := strings.Split(lConfig.Peer2, ".")

		peer1Idx, _ := strconv.Atoi(peer1[1])
		peer2Idx, _ := strconv.Atoi(peer2[1])

		t.links = append(t.links, &NetemLink{
			Peer1: NetemLinkPeer{
				Node:    t.GetNode(peer1[0]),
				IfIndex: peer1Idx,
			},
			Peer2: NetemLinkPeer{
				Node:    t.GetNode(peer2[0]),
				IfIndex: peer2Idx,
			},
		})
	}

	return nil
}

func (t *NetemTopologyManager) Reload() error {
	if err := t.Close(); err != nil {
		return err
	}

	if err := t.Load(); err != nil {
		return err
	}
	if t.running {
		return t.Run()
	}

	return nil
}

func (t *NetemTopologyManager) Run() error {
	var err error
	if t.running {
		t.logger.Warn("Topology is already running")
		return nil
	}

	g := new(errgroup.Group)
	// 1 - start ovswitch container and init p2pSwitch
	t.ovsInstance.Start()
	if err != nil {
		return err
	}

	// 2 - start all nodes
	for _, node := range t.nodes {
		node := node
		g.Go(func() error { return node.Start() })
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// 3 - create links
	for _, l := range t.links {
		veth, err := link.CreateVethLink(
			genIfName(t.prjID, l.Peer1.Node.GetName(), l.Peer1.IfIndex, l.Peer2.Node.GetName(), l.Peer2.IfIndex),
			genIfName(t.prjID, l.Peer2.Node.GetName(), l.Peer2.IfIndex, l.Peer1.Node.GetName(), l.Peer1.IfIndex),
		)
		if err != nil {
			return err
		}

		if err := l.Peer1.Node.AttachInterface(veth.Name, l.Peer1.IfIndex); err != nil {
			return err
		}
		if err := l.Peer2.Node.AttachInterface(veth.PeerName, l.Peer2.IfIndex); err != nil {
			return err
		}
	}

	// 4 - load configs
	configPath := path.Join(t.path, configDir)
	for _, node := range t.nodes {
		node := node
		g.Go(func() error { return node.LoadConfig(configPath) })
	}
	if err := g.Wait(); err != nil {
		return err
	}

	t.running = true
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

func (t *NetemTopologyManager) startNode(node INetemNode) error {
	if err := node.Start(); err != nil {
		return fmt.Errorf("Unable to start node %s: %w", node.GetName(), err)
	}

	configPath := path.Join(t.path, configDir)
	if err := node.LoadConfig(configPath); err != nil {
		return fmt.Errorf("Unable to load config of node %s: %w", node.GetName(), err)
	}

	return nil
}

func (t *NetemTopologyManager) stopNode(node INetemNode) error {
	if err := node.Stop(); err != nil {
		return fmt.Errorf("Unable to stop node %s: %w", node.GetName(), err)
	}
	return nil
}

func (t *NetemTopologyManager) Start(nodeName string) error {
	node := t.GetNode(nodeName)
	if node == nil {
		return fmt.Errorf("Node %s not found in the topology", nodeName)
	}

	return t.startNode(node)
}

func (t *NetemTopologyManager) Stop(nodeName string) error {
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

func (t *NetemTopologyManager) Copy(source, dest string) error {
	var container INetemNode
	srcContainer, srcPath := splitCopyArg(source)
	destContainer, destPath := splitCopyArg(dest)

	var direction copyDirection
	if srcContainer != "" {
		direction |= fromContainer
		container = t.GetNode(srcContainer)
		if container == nil {
			return fmt.Errorf("Node %s not found in the topology", srcContainer)
		}
	}
	if destContainer != "" {
		direction |= toContainer
		container = t.GetNode(destContainer)
		if container == nil {
			return fmt.Errorf("Node %s not found in the topology", destContainer)
		}
	}

	if container.GetType() != "docker" {
		return errors.New("Selected node does not support copy")
	}
	dockerNode := container.(*docker.DockerNode)

	switch direction {
	case fromContainer:
		return dockerNode.CopyFrom(srcPath, destPath)
	case toContainer:
		return dockerNode.CopyTo(srcPath, destPath)
	case acrossContainers:
		return errors.New("copying between containers is not supported")
	default:
		return errors.New("must specify at least one container source")
	}
}

func (t *NetemTopologyManager) Close() error {
	for _, node := range t.nodes {
		if node != nil {
			if err := node.Close(); err != nil {
				t.logger.Errorf("Error when closing node %s: %v", node.GetName(), err)
			}
		}
	}
	t.nodes = make([]INetemNode, 0)

	if err := ovs.CloseOvsInstance(t.prjID); err != nil {
		t.logger.Errorf("Error when closing ovwitch instance: %v", err)
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
	}
	if err := topo.Load(); err != nil {
		return topo, fmt.Errorf("Unable to load the topology:\n\t%w", err)
	}
	return topo, nil
}
