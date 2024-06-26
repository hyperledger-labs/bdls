/*
Copyright @Ahmed Al Salih. @BDLS @UNCC All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bdls

import (
	"bytes"
	"path"
	"reflect"

	"code.cloudfoundry.org/clock"
	"github.com/hyperledger/fabric-protos-go/common"

	//cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/msp"
	ab "github.com/hyperledger/fabric-protos-go/orderer"

	"github.com/pkg/errors"

	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/crypto"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/metrics"
	"github.com/hyperledger/fabric/common/policies"
	"github.com/hyperledger/fabric/internal/pkg/comm"
	"github.com/hyperledger/fabric/orderer/common/cluster"
	"github.com/hyperledger/fabric/orderer/common/localconfig"
	"github.com/hyperledger/fabric/orderer/common/multichannel"
	"github.com/hyperledger/fabric/orderer/consensus"
	"github.com/hyperledger/fabric/protoutil"

	//"google.golang.org/protobuf/proto"
	"github.com/golang/protobuf/proto"
)

// Config contains bdls configurations
type Config struct {
}

// ChainGetter obtains instances of ChainSupport for the given channel
type ChainGetter interface {
	// GetChain obtains the ChainSupport for the given channel.
	// Returns nil, false when the ChainSupport for the given channel
	// isn't found.
	GetChain(chainID string) *multichannel.ChainSupport
}

// PolicyManagerRetriever is the policy manager retriever function
type PolicyManagerRetriever func(channel string) policies.Manager

// Consenter implements bdls consenter
type Consenter struct {
	CreateChain      func(chainName string)
	GetPolicyManager PolicyManagerRetriever
	Logger           *flogging.FabricLogger
	Identity         []byte
	Comm             *cluster.AuthCommMgr
	Chains           ChainGetter
	SignerSerializer SignerSerializer
	Registrar        *multichannel.Registrar
	WALBaseDir       string
	ClusterDialer    *cluster.PredicateDialer
	Conf             *localconfig.TopLevel
	Metrics          *Metrics
	BCCSP            bccsp.BCCSP
	ClusterService   *cluster.ClusterService
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

	puller, err := newBlockPuller(support, c.ClusterDialer, c.Conf.General.Cluster, c.BCCSP)
	if err != nil {
		c.Logger.Panicf("Failed initializing block puller")
	}

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
		Consenters:        consenters,
		MaxInflightBlocks: 1,
		Clock:             clock.NewClock(),
	}

	chain, err := NewChain(
		//configValidator,
		(uint64)(selfID),
		//config,

		path.Join(c.WALBaseDir, support.ChannelID()),
		puller,
		c.Comm,
		c.SignerSerializer,
		c.GetPolicyManager(support.ChannelID()),
		support,
		c.Metrics,
		c.BCCSP,
		opts,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed creating a new Chain")
	}
	chain.opts = opts

	// refresh cluster service with updated consenters
	c.ClusterService.ConfigureNodeCerts(chain.Channel, consenters)
	chain.clusterService = c.ClusterService

	return chain, nil
}

func New(
	pmr PolicyManagerRetriever,
	signerSerializer SignerSerializer,
	clusterDialer *cluster.PredicateDialer,
	conf *localconfig.TopLevel,
	srvConf comm.ServerConfig, // TODO why is this not used?
	srv *comm.GRPCServer,
	r *multichannel.Registrar,
	metricsProvider metrics.Provider,
	clusterMetrics *cluster.Metrics,
	BCCSP bccsp.BCCSP,
) *Consenter {
	logger := flogging.MustGetLogger("orderer.consensus.bdls")

	//var walConfig WALConfig

	logger.Infof("Starting NEW bdls.....U-N-C-C****/////.......")
	consenter := &Consenter{
		Registrar:        r,
		GetPolicyManager: pmr,
		Conf:             conf,
		ClusterDialer:    clusterDialer,
		Logger:           logger,
		Chains:           r,
		SignerSerializer: signerSerializer,
		Metrics:          NewMetrics(metricsProvider),
		CreateChain:      r.CreateChain,
		BCCSP:            BCCSP,
	}

	identity, _ := signerSerializer.Serialize()
	sID := &msp.SerializedIdentity{}
	if err := proto.Unmarshal(identity, sID); err != nil {
		logger.Panicf("failed unmarshaling identity: %s", err)
	}

	consenter.Identity = sID.IdBytes

	consenter.Comm = &cluster.AuthCommMgr{
		Logger:         flogging.MustGetLogger("orderer.common.cluster"),
		Metrics:        clusterMetrics,
		SendBufferSize: conf.General.Cluster.SendBufferSize,
		Chan2Members:   make(cluster.MembersByChannel),
		Connections:    cluster.NewConnectionMgr(clusterDialer.Config),
		Signer:         signerSerializer,
		NodeIdentity:   sID.IdBytes,
	}

	consenter.ClusterService = &cluster.ClusterService{
		StreamCountReporter: &cluster.StreamCountReporter{
			Metrics: clusterMetrics,
		},
		Logger:                           flogging.MustGetLogger("orderer.common.cluster"),
		StepLogger:                       flogging.MustGetLogger("orderer.common.cluster.step"),
		MinimumExpirationWarningInterval: cluster.MinimumExpirationWarningInterval,
		CertExpWarningThreshold:          conf.General.Cluster.CertExpirationWarningThreshold,
		MembershipByChannel:              make(map[string]*cluster.ChannelMembersConfig),
		NodeIdentity:                     sID.IdBytes,
		RequestHandler: &Ingress{
			Logger:        logger,
			ChainSelector: consenter,
		},
	}

	ab.RegisterClusterNodeServiceServer(srv.Server(), consenter.ClusterService)

	return consenter
}

// ReceiverGetter obtains instances of MessageReceiver given a channel ID
// type ReceiverGetter interface  must implement this interface function in consenter
// ReceiverByChain returns the MessageReceiver for the given channelID or nil if not found.
func (c *Consenter) ReceiverByChain(channelID string) MessageReceiver {
	cs := c.Chains.GetChain(channelID)
	if cs == nil {
		return nil
	}
	if cs.Chain == nil {
		c.Logger.Panicf("Programming error - Chain %s is nil although it exists in the mapping", channelID)
	}
	if bdlsChain, isBDLS := cs.Chain.(*Chain); isBDLS {
		return bdlsChain // error if not implement the MessageReceiver interface functions
		// in chain.go (HandleMessage & HandleRequest)
	}
	c.Logger.Warningf("Chain %s is of type %v and not bdls.Chain", channelID, reflect.TypeOf(cs.Chain))
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
		santizedCert, err := crypto.SanitizeX509Cert(consenter.Identity)
		if err != nil {
			return false, err
		}
		if bytes.Equal(c.Identity, santizedCert) {
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
	case *ab.ConsensusRequest:
		return req.Channel
	case *ab.SubmitRequest:
		return req.Channel
	default:
		return ""
	}
}

func (c *Consenter) detectSelfID(consenters []*common.Consenter) (uint32, error) {
	for _, cst := range consenters {
		santizedCert, err := crypto.SanitizeX509Cert(cst.Identity)
		if err != nil {
			return 0, err
		}
		if bytes.Equal(c.Comm.NodeIdentity, santizedCert) {
			return cst.Id, nil
		}
	}
	c.Logger.Warning("Could not find the node in channel consenters set")
	return 0, cluster.ErrNotInChannel
}
