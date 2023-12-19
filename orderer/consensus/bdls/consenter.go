/*
Copyright @Ahmed Al Salih. @BDLS @UNCC All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bdls

import (
	"reflect"

	"code.cloudfoundry.org/clock"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/orderer"
	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/crypto"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/metrics"
	"github.com/hyperledger/fabric/internal/pkg/comm"
	"github.com/hyperledger/fabric/orderer/common/cluster"
	"github.com/hyperledger/fabric/orderer/common/localconfig"
	"github.com/hyperledger/fabric/orderer/common/types"
	"github.com/hyperledger/fabric/orderer/consensus"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"

	//"google.golang.org/protobuf/proto"
	"github.com/golang/protobuf/proto"
)

// ChainManager defines the methods from multichannel.Registrar needed by the Consenter.
type ChainManager interface {
	GetConsensusChain(channelID string) consensus.Chain
	CreateChain(channelID string)
	SwitchChainToFollower(channelID string)
	ReportConsensusRelationAndStatusMetrics(channelID string, relation types.ConsensusRelation, status types.Status)
}

// Config contains etcdraft configurations
type Config struct {
	WALDir               string // WAL data of <my-channel> is stored in WALDir/<my-channel>
	SnapDir              string // Snapshots of <my-channel> are stored in SnapDir/<my-channel>
	EvictionSuspicion    string // Duration threshold that the node samples in order to suspect its eviction from the channel.
	TickIntervalOverride string // Duration to use for tick interval instead of what is specified in the channel config.
}

// Consenter implements etcdraft consenter
type Consenter struct {
	ChainManager  ChainManager
	Dialer        *cluster.PredicateDialer
	Communication cluster.Communicator
	*Dispatcher
	Logger         *flogging.FabricLogger
	EtcdRaftConfig Config
	OrdererConfig  localconfig.TopLevel
	Cert           []byte
	Metrics        *Metrics
	BCCSP          bccsp.BCCSP
}

// HandleChain returns a new Chain instance or an error upon failure
func (c *Consenter) HandleChain(support consensus.ConsenterSupport, metadata *common.Metadata) (consensus.Chain, error) {
	//configOptions := &smartbft.Options{}
	consenters := support.SharedConfig().Consenters()
	/*if err := proto.Unmarshal(support.SharedConfig().ConsensusMetadata(), configOptions); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal consensus metadata")
	}*/

	selfID, err := c.detectSelfID(consenters)
	if err != nil {
		return nil, errors.Wrap(err, "without a system channel, a follower should have been created")
	}
	c.Logger.Infof("Local consenter id is %d", selfID)

	// puller, err := newBlockPuller(support, c.ClusterDialer, c.Conf.General.Cluster, c.BCCSP)
	// if err != nil {
	// 	c.Logger.Panicf("Failed initializing block puller")
	// }

	//config, err := configFromMetadataOptions((uint64)(selfID), configOptions)
	if err != nil {
		return nil, errors.Wrap(err, "failed parsing smartbft configuration")
	}
	//c.Logger.Debugf("SmartBFT-Go config: %+v", config)
	/*
		configValidator := &ConfigBlockValidator{
			ValidatingChannel:    support.ChannelID(),
			Filters:              c.Registrar,
			ConfigUpdateProposer: c.Registrar,
			Logger:               c.Logger,
		}
	*/
	opts := Options{
		RPCTimeout:        c.OrdererConfig.General.Cluster.RPCTimeout,
		Consenters:        consenters,
		MaxInflightBlocks: 1,

		Logger:  c.Logger,
		Cert:    c.Cert,
		Metrics: c.Metrics,
		Clock:   clock.NewClock(),
	}

	rpc := &cluster.RPC{
		Timeout:       c.OrdererConfig.General.Cluster.RPCTimeout,
		Logger:        c.Logger,
		Channel:       support.ChannelID(),
		Comm:          c.Communication,
		StreamsByType: cluster.NewStreamsByType(),
	}
	haltCallback := func() { c.ChainManager.SwitchChainToFollower(support.ChannelID()) }

	return NewChain(
		//configValidator,
		(uint64)(selfID),
		//config,

		support,
		opts,
		c.Communication,
		rpc,
		c.BCCSP,
		func() (BlockPuller, error) {
			return NewBlockPuller(support, c.Dialer, c.OrdererConfig.General.Cluster, c.BCCSP)
		},
		haltCallback,
		nil,
	)
}

// New creates a etcdraft Consenter
func New(
	clusterDialer *cluster.PredicateDialer,
	conf *localconfig.TopLevel,
	srvConf comm.ServerConfig,
	srv *comm.GRPCServer,
	registrar ChainManager,
	metricsProvider metrics.Provider,
	bccsp bccsp.BCCSP,
) *Consenter {
	logger := flogging.MustGetLogger("orderer.consensus.etcdraft")

	var cfg Config
	err := mapstructure.Decode(conf.Consensus, &cfg)
	if err != nil {
		logger.Panicf("Failed to decode etcdraft configuration: %s", err)
	}

	consenter := &Consenter{
		ChainManager:   registrar,
		Cert:           srvConf.SecOpts.Certificate,
		Logger:         logger,
		EtcdRaftConfig: cfg,
		OrdererConfig:  *conf,
		Dialer:         clusterDialer,
		Metrics:        NewMetrics(metricsProvider),
		BCCSP:          bccsp,
	}
	consenter.Dispatcher = &Dispatcher{
		Logger:        logger,
		ChainSelector: consenter,
	}

	comm := createComm(clusterDialer, consenter, conf.General.Cluster, metricsProvider)
	consenter.Communication = comm
	svc := &cluster.Service{
		CertExpWarningThreshold:          conf.General.Cluster.CertExpirationWarningThreshold,
		MinimumExpirationWarningInterval: cluster.MinimumExpirationWarningInterval,
		StreamCountReporter: &cluster.StreamCountReporter{
			Metrics: comm.Metrics,
		},
		StepLogger: flogging.MustGetLogger("orderer.common.cluster.step"),
		Logger:     flogging.MustGetLogger("orderer.common.cluster"),
		Dispatcher: comm,
	}
	orderer.RegisterClusterServer(srv.Server(), svc)

	return consenter //, comm.Metrics
}

func createComm(clusterDialer *cluster.PredicateDialer, c *Consenter, config localconfig.Cluster, p metrics.Provider) *cluster.Comm {
	metrics := cluster.NewMetrics(p)
	logger := flogging.MustGetLogger("orderer.common.cluster")

	compareCert := cluster.CachePublicKeyComparisons(func(a, b []byte) bool {
		err := crypto.CertificatesWithSamePublicKey(a, b)
		if err != nil && err != crypto.ErrPubKeyMismatch {
			crypto.LogNonPubKeyMismatchErr(logger.Errorf, err, a, b)
		}
		return err == nil
	})

	comm := &cluster.Comm{
		MinimumExpirationWarningInterval: cluster.MinimumExpirationWarningInterval,
		CertExpWarningThreshold:          config.CertExpirationWarningThreshold,
		SendBufferSize:                   config.SendBufferSize,
		Logger:                           logger,
		Chan2Members:                     make(map[string]cluster.MemberMapping),
		Connections:                      cluster.NewConnectionStore(clusterDialer, metrics.EgressTLSConnectionCount),
		Metrics:                          metrics,
		ChanExt:                          c,
		H:                                c,
		CompareCertificate:               compareCert,
	}
	c.Communication = comm
	return comm
}

// ReceiverGetter obtains instances of MessageReceiver given a channel ID
// type ReceiverGetter interface  must implement this interface function in consenter
// ReceiverByChain returns the MessageReceiver for the given channelID or nil if not found.
func (c *Consenter) ReceiverByChain(channelID string) MessageReceiver {
	chain := c.ChainManager.GetConsensusChain(channelID)
	if chain == nil {
		return nil
	}
	if bdlsChain, isBDLS := chain.(*Chain); isBDLS {
		return bdlsChain // error will occured if chain not implement the MessageReceiver interface functions
		// In chain.go (HandleMessage & HandleRequest) for Orderer 3.0
		// Or Submit() & Consensus() functions.
	}
	c.Logger.Warningf("Chain %s is of type %v and not bdls.Chain", channelID, reflect.TypeOf(chain))
	return nil
}

func (c *Consenter) IsChannelMember(joinBlock *common.Block) (bool, error) {
	if joinBlock == nil {
		return false, errors.New("nil block")
	}
	envelopeConfig, err := protoutil.ExtractEnvelope(joinBlock, 0)
	if err != nil {
		return false, err
	}
	bundle, err := channelconfig.NewBundleFromEnvelope(envelopeConfig, c.BCCSP)
	if err != nil {
		return false, err
	}
	oc, exists := bundle.OrdererConfig()
	if !exists {
		return false, errors.New("no orderer config in bundle")
	}
	member := false
	for _, consenter := range oc.Consenters() {
		//santizedCert, err := crypto.SanitizeX509Cert(consenter.Identity)
		if err != nil {
			return false, err
		}
		if nil != consenter {
			member = true
			break
		}
	}

	return member, nil
}

// TargetChannel extracts the channel from the given proto.Message.
// Returns an empty string on failure.
func (c *Consenter) TargetChannel(message proto.Message) string {
	switch req := message.(type) {
	case *orderer.ConsensusRequest:
		return req.Channel
	case *orderer.SubmitRequest:
		return req.Channel
	default:
		return ""
	}
}

func (c *Consenter) detectSelfID(consenters []*common.Consenter) (uint64, error) {
	thisNodeCertAsDER, err := pemToDER(c.Cert, 0, "server", c.Logger)
	if err != nil {
		return 0, err
	}

	var serverCertificates []string
	for nodeID, cst := range consenters {
		serverCertificates = append(serverCertificates, string(cst.ServerTlsCert))

		certAsDER, err := pemToDER(cst.ServerTlsCert, uint64(nodeID), "server", c.Logger)
		if err != nil {
			return 0, err
		}

		if crypto.CertificatesWithSamePublicKey(thisNodeCertAsDER, certAsDER) == nil {
			return uint64(nodeID), nil
		}
	}

	c.Logger.Warning("Could not find", string(c.Cert), "among", serverCertificates)
	return 0, cluster.ErrNotInChannel
}
