/*
Copyright Ahmed Al Salih. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bdls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/clock"
	"github.com/BDLS-bft/bdls"
	"github.com/hyperledger/fabric-protos-go/common"
	"go.etcd.io/etcd/raft/v3"

	//cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/orderer"
	"github.com/hyperledger/fabric-protos-go/orderer/etcdraft"
	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/policies"
	"github.com/hyperledger/fabric/orderer/common/cluster"
	"github.com/hyperledger/fabric/orderer/consensus"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/BDLS-bft/bdls/crypto/btcec"
	agent "github.com/hyperledger/fabric/orderer/consensus/bdls/agent-tcp"
)

// ConfigValidator interface
type ConfigValidator interface {
	ValidateConfig(env *common.Envelope) error
}

//go:generate mockery -dir . -name RPC -case underscore -output mocks

// RPC sends a consensus and submits a request
type RPC interface {
	SendConsensus(dest uint64, msg *orderer.ConsensusRequest) error
	// SendSubmit(dest uint64, request *orderer.SubmitRequest) error
	SendSubmit(dest uint64, request *orderer.SubmitRequest, report func(error)) error
}

type BlockPuller interface {
	PullBlock(seq uint64) *common.Block
	HeightsByEndpoints() (map[string]uint64, error)
	Close()
}

// secp256k1 elliptic curve
var S256Curve elliptic.Curve = btcec.S256()

const (
	baseLatency               = 500 * time.Millisecond
	maxBaseLatency            = 10 * time.Second
	proposalCollectionTimeout = 3 * time.Second
	updatePeriod              = 20 * time.Millisecond
	resendPeriod              = 10 * time.Second
)

type signerSerializer interface {
	// Sign a message and return the signature over the digest, or error on failure
	Sign(message []byte) ([]byte, error)

	// Serialize converts an identity to bytes
	Serialize() ([]byte, error)
}

type submit struct {
	req *orderer.SubmitRequest
	//leader chan uint64
}

type apply struct {
	height uint64
	round  uint64
	state  bdls.State
}

// Chain represents a BDLS chain.
type Chain struct {
	configurator Configurator
	bdlsId       uint64
	Channel      string

	rpc RPC

	clock clock.Clock // Tests can inject a fake clock

	//agent *agent

	//BDLS
	consensus           *bdls.Consensus
	config              *bdls.Config
	consensusMessages   [][]byte      // all consensus message awaiting to be processed
	sync.Mutex                        // fields lock
	chConsensusMessages chan struct{} // notification of new consensus message

	submitC chan *submit
	applyC  chan apply
	haltC   chan struct{} // Signals to goroutines that the chain is halting
	doneC   chan struct{} // Closes when the chain halts
	startC  chan struct{} // Closes when the node is started

	errorCLock   sync.RWMutex
	errorC       chan struct{} // returned by Errored()
	haltCallback func()

	Logger  *flogging.FabricLogger
	support consensus.ConsenterSupport
	//verifier *Verifier
	opts Options

	lastBlock *common.Block
	//TBD
	RuntimeConfig *atomic.Value

	//Config           types.Configuration
	BlockPuller      BlockPuller
	Comm             cluster.Communicator
	SignerSerializer signerSerializer
	PolicyManager    policies.Manager

	WALDir string

	clusterService *cluster.ClusterService

	//assembler *Assembler
	Metrics *Metrics
	// BCCSP instance
	CryptoProvider bccsp.BCCSP

	statusReportMutex sync.Mutex
	bdlsChainLock     sync.RWMutex

	unreachableLock sync.RWMutex
	unreachable     map[uint64]struct{}

	//statusReportMutex sync.Mutex
	//consensusRelation types2.ConsensusRelation
	//status            types2.Status

	configInflight bool // this is true when there is config block or ConfChange in flight
	blockInflight  int  // number of in flight blocks
	transportLayer *agent.TCPAgent

	latency      time.Duration
	die          chan struct{}
	dieOnce      sync.Once
	msgCount     int64
	bytesCount   int64
	minLatency   time.Duration
	maxLatency   time.Duration
	totalLatency time.Duration

	Node      node
	Endpoint  string
	Endpoints []string
	TPS       float64
	start     time.Time
	end       time.Time
}

// Consensus implements MessageReceiver.
func (c *Chain) Consensus(req *orderer.ConsensusRequest, sender uint64) error {
	c.Logger.Debugf("in Chain.go Consensus()  Message from %d", sender)
	/*date, err := proto.Marshal(m)
	if err != nil {
		c.Logger.Info(err)
	}*/
	c.Logger.Info("DDDDDDDDDDDDDDDDDDDDDD ", req.Payload)
	bdlsmsgorstate, err := protoutil.UnmarshalBlock(req.Payload)
	if err != nil {
		return errors.Errorf("failed to unmarshal bdls State to block: %s", err)
	}

	c.Logger.Info("DDDDDDDDDDDDDDDDDDDDDD ", bdlsmsgorstate)

	c.consensus.ReceiveMessage(req.Payload, time.Now())
	return nil
}

// Submit implements MessageReceiver.

type Options struct {
	RPCTimeout time.Duration
	Clock      clock.Clock
	Logger     *flogging.FabricLogger
	Metrics    *Metrics
	Cert       []byte

	BlockMetadata *etcdraft.BlockMetadata

	// BlockMetadata and Consenters should only be modified while under lock
	// of bdlsChainLock
	//Consenters    map[uint64]*etcdraft.Consenter
	Consenters []*common.Consenter

	portAddress string

	MaxInflightBlocks int
}

// Order accepts a message which has been processed at a given configSeq.
func (c *Chain) Order(env *common.Envelope, configSeq uint64) error {

	seq := c.support.Sequence()
	if configSeq < seq {
		c.Logger.Warnf("Normal message was validated against %d, although current config seq has advanced (%d)", configSeq, seq)
		if _, err := c.support.ProcessNormalMsg(env); err != nil {
			return errors.Errorf("bad normal message: %s", err)
		}
	}

	return c.Submit(&orderer.SubmitRequest{LastValidationSeq: configSeq, Payload: env, Channel: c.Channel}, 0)
}

// Configure accepts a message which reconfigures the channel
func (c *Chain) Configure(env *common.Envelope, configSeq uint64) error {
	seq := c.support.Sequence()
	if configSeq < seq {
		c.Logger.Warnf("Normal message was validated against %d, although current config seq has advanced (%d)", configSeq, seq)
		if configEnv, _, err := c.support.ProcessConfigMsg(env); err != nil {
			return errors.Errorf("bad normal message: %s", err)
		} else {
			return c.Submit(&orderer.SubmitRequest{LastValidationSeq: configSeq, Payload: configEnv, Channel: c.Channel}, 0)
		}
	}
	return c.Submit(&orderer.SubmitRequest{LastValidationSeq: configSeq, Payload: env, Channel: c.Channel}, 0)
}

// Submit implements MessageReceiver.
func (c *Chain) Submit(req *orderer.SubmitRequest, sender uint64) error {
	select {
	case c.submitC <- &submit{req}:
		return nil
	case <-c.doneC:
		c.Metrics.ProposalFailures.Add(1)
		return errors.Errorf("chain is stopped")
	}
}

// WaitReady blocks waiting for consenter to be ready for accepting new messages.
func (c *Chain) WaitReady() error {
	if err := c.isRunning(); err != nil {
		return err
	}

	select {
	case c.submitC <- nil:
	case <-c.doneC:
		return errors.Errorf("chain is stopped")
	}
	return nil
}

// Errored returns a channel which will close when an error has occurred.
func (c *Chain) Errored() <-chan struct{} {
	//TODO
	return nil
}

// Configurator is used to configure the communication layer
// when the chain starts.
type Configurator interface {
	Configure(channel string, newNodes []cluster.RemoteNode)
}

// CreateBlockPuller is a function to create BlockPuller on demand.
// It is passed into chain initializer so that tests could mock this.
type CreateBlockPuller func() (BlockPuller, error)

// NewChain creates new chain
func NewChain(
	//cv ConfigValidator,
	selfID uint64,
	support consensus.ConsenterSupport,
	opts Options,
	conf Configurator,
	rpc RPC,
	cryptoProvider bccsp.BCCSP,
	f CreateBlockPuller,
	haltCallback func(),
	observeC chan<- raft.SoftState,

) (*Chain, error) {
	/*requestInspector := &RequestInspector{
		ValidateIdentityStructure: func(_ *msp.SerializedIdentity) error {
			return nil
		},
	}*/

	logger := flogging.MustGetLogger("orderer.consensus.bdls.chain").With(zap.String("channel", support.ChannelID()))
	b := support.Block(support.Height() - 1)

	if b == nil {
		return nil, errors.Errorf("failed to get last block")
	}

	c := &Chain{
		configurator: conf,
		//RuntimeConfig:     &atomic.Value{},
		Channel: support.ChannelID(),
		//Config:            config,
		rpc:       rpc,
		lastBlock: b,
		//WALDir:    walDir,
		//Comm:      comm,
		support: support,

		haltCallback: haltCallback,
		//BlockPuller:  blockPuller,
		Logger: logger,
		opts:   opts,
		bdlsId: selfID,
		clock:  opts.Clock,

		applyC:  make(chan apply),
		submitC: make(chan *submit),
		haltC:   make(chan struct{}),
		doneC:   make(chan struct{}),
		startC:  make(chan struct{}),
		errorC:  make(chan struct{}),

		Metrics: &Metrics{
			ClusterSize:          opts.Metrics.ClusterSize.With("channel", support.ChannelID()),
			CommittedBlockNumber: opts.Metrics.CommittedBlockNumber.With("channel", support.ChannelID()),
			IsLeader:             opts.Metrics.IsLeader.With("channel", support.ChannelID()),
			LeaderID:             opts.Metrics.LeaderID.With("channel", support.ChannelID()),
		},
		CryptoProvider: cryptoProvider,

		chConsensusMessages: make(chan struct{}, 1),
	}
	/*
		lastBlock := LastBlockFromLedgerOrPanic(support, c.Logger)
		lastConfigBlock := LastConfigBlockFromLedgerOrPanic(support, c.Logger)

		}*/

	/*privateKey, err := ecdsa.GenerateKey(S256Curve, rand.Reader)
	if err != nil {
		c.Logger.Warnf("error generating privateKey value:", err)
	}*/

	// setup consensus config at the given height
	config := &bdls.Config{
		Epoch:         time.Now(),
		CurrentHeight: c.lastBlock.Header.Number, //support.Height() - 1, //0,
		StateCompare:  func(a bdls.State, b bdls.State) int { return bytes.Compare(a, b) },
		StateValidate: func(bdls.State) bool { return true },
	}
	/*config := new(bdls.Config)
	config.Epoch = time.Now()
	config.CurrentHeight = 0 // c.support.Height()
	config.StateCompare = func(a bdls.State, b bdls.State) int { return bytes.Compare(a, b) }
	config.StateValidate = func(bdls.State) bool { return true }
	*/
	Keys := make([]string, 0)
	Keys = append(Keys,
		"68082493172628484253808951113461196766221768923883438540199548009461479956986",
		"44652770827640294682875208048383575561358062645764968117337703282091165609211",
		"80512969964988849039583604411558290822829809041684390237207179810031917243659",
		"55978351916851767744151875911101025920456547576858680756045508192261620541580")
	for k := range Keys { //c.opts.Consenters {
		//for k := range c.opts.Consenters {
		i := new(big.Int)
		_, err := fmt.Sscan(Keys[k], i)
		if err != nil {
			c.Logger.Warnf("error scanning value:", err)
		}
		priv := new(ecdsa.PrivateKey)
		priv.PublicKey.Curve = bdls.S256Curve
		priv.D = i
		priv.PublicKey.X, priv.PublicKey.Y = bdls.S256Curve.ScalarBaseMult(priv.D.Bytes())
		// myself
		logger.Info("XXXXXXX ", c.bdlsId, k, "  XXXXXXXX ")
		if int(c.bdlsId) == k {
			logger.Info("----- T r u e -----")
			config.PrivateKey = priv
		}

		// set validator sequence
		config.Participants = append(config.Participants, bdls.DefaultPubKeyToIdentity(&priv.PublicKey))
	}

	c.config = config

	//Get the []cluster.RemoteNode for other Orderer nodes.
	/*nodes, err := c.remotePeers()
	if err != nil {
		return nil, errors.WithStack(err)
	}*/

	/*lastBlock := LastBlockFromLedgerOrPanic(support, c.Logger)
	lastConfigBlock := LastConfigBlockFromLedgerOrPanic(support, c.Logger)

	rtc := RuntimeConfig{
		logger: logger,
		id:     selfID,
	}
	rtc, err := rtc.BlockCommitted(lastConfigBlock, bccsp)
	if err != nil {
		return nil, errors.Wrap(err, "failed constructing RuntimeConfig")
	}
	rtc, err = rtc.BlockCommitted(lastBlock, bccsp)
	if err != nil {
		return nil, errors.Wrap(err, "failed constructing RuntimeConfig")
	}

	c.RuntimeConfig.Store(rtc)*/

	//Configure the connection for All orderer nodes.
	//c.Comm.Configure(c.support.ChannelID(), nodes)

	/*	if err := c.consensus.ValidateConfiguration(rtc.Nodes); err != nil {
			return nil, errors.Wrap(err, "failed to verify SmartBFT-Go configuration")
		}
	*/
	logger.Infof("BDLS is now servicing chain %s", support.ChannelID())

	return c, nil
}

// Halt frees the resources which were allocated for this Chain.
func (c *Chain) Halt() {

	//TODO
}

// Get the remote peers from the []*cb.Consenter
func (c *Chain) remotePeers() ([]cluster.RemoteNode, error) {
	c.bdlsChainLock.RLock()
	defer c.bdlsChainLock.RUnlock()

	var nodes []cluster.RemoteNode
	for id, consenter := range c.opts.Consenters {
		// No need to know yourself
		if uint64(id) == c.bdlsId {
			//c.opts.portAddress = fmt.Sprint(consenter.Port)
			continue
		}
		serverCertAsDER, err := pemToDER(consenter.ServerTlsCert, uint64(id), "server", c.Logger)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		clientCertAsDER, err := pemToDER(consenter.ClientTlsCert, uint64(id), "client", c.Logger)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		c.Logger.Info("XXXXXXXXXXX remotePeers XXXXXXXXXX: ", fmt.Sprintf("%s:%d", consenter.Host, consenter.Port), " :XXXXXXXXXX")
		nodes = append(nodes, cluster.RemoteNode{
			NodeAddress: cluster.NodeAddress{
				ID:       uint64(id),
				Endpoint: fmt.Sprintf("%s:%d", consenter.Host, consenter.Port),
			},
			NodeCerts: cluster.NodeCerts{
				ServerTLSCert: serverCertAsDER,
				ClientTLSCert: clientCertAsDER,
			},
		})
		//c.Logger.Infof("BDLS Node ID from the remotePeers(): %s ------------", nodes[0].ID)
	}

	return nodes, nil
}

func pemToDER(pemBytes []byte, id uint64, certType string, logger *flogging.FabricLogger) ([]byte, error) {
	bl, _ := pem.Decode(pemBytes)
	if bl == nil {
		logger.Errorf("Rejecting PEM block of %s TLS cert for node %d, offending PEM is: %s", certType, id, string(pemBytes))
		return nil, errors.Errorf("invalid PEM block")
	}
	return bl.Bytes, nil
}

// publicKeyFromCertificate returns the public key of the given ASN1 DER certificate.
func publicKeyFromCertificate(der []byte) ([]byte, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKIXPublicKey(cert.PublicKey)
}

// Orders the envelope in the `msg` content. SubmitRequest.
// Returns
//
//	-- batches [][]*common.Envelope; the batches cut,
//	-- pending bool; if there are envelopes pending to be ordered,
//	-- err error; the error encountered, if any.
//
// It takes care of config messages as well as the revalidation of messages if the config sequence has advanced.
func (c *Chain) ordered(msg *orderer.SubmitRequest) (batches [][]*common.Envelope, pending bool, err error) {
	seq := c.support.Sequence()

	isconfig, err := c.isConfig(msg.Payload)
	if err != nil {
		return nil, false, errors.Errorf("bad message: %s", err)
	}

	if isconfig {
		// ConfigMsg
		if msg.LastValidationSeq < seq {
			c.Logger.Warnf("Config message was validated against %d, although current config seq has advanced (%d)", msg.LastValidationSeq, seq)
			msg.Payload, _, err = c.support.ProcessConfigMsg(msg.Payload)
			if err != nil {
				//c.Metrics.ProposalFailures.Add(1)
				return nil, true, errors.Errorf("bad config message: %s", err)
			}
		}
		/*
			if c.checkForEvictionNCertRotation(msg.Payload) {

				if !atomic.CompareAndSwapUint32(&c.leadershipTransferInProgress, 0, 1) {
					c.logger.Warnf("A reconfiguration transaction is already in progress, ignoring a subsequent transaction")
					return
				}

				go func() {
					defer atomic.StoreUint32(&c.leadershipTransferInProgress, 0)

					abdicated := false
					for attempt := 1; attempt <= AbdicationMaxAttempts; attempt++ {
						if err := c.Node.abdicateLeadership(); err != nil {
							// If there is no leader, abort and do not retry.
							// Return early to prevent re-submission of the transaction
							if err == ErrNoLeader || err == ErrChainHalting {
								c.Logger.Warningf("Abdication attempt no.%d failed because there is no leader or chain halting, will not try again, will not submit TX, error: %s", attempt, err)
								return
							}

							// If the error isn't any of the below, it's a programming error, so panic.
							if err != ErrNoAvailableLeaderCandidate && err != ErrTimedOutLeaderTransfer {
								c.Logger.Panicf("Programming error, abdicateLeader() returned with an unexpected error: %s", err)
							}

							// Else, it's one of the errors above, so we retry.
							c.Logger.Warningf("Abdication attempt no.%d failed, will try again, error: %s", attempt, err)
							continue
						} else {
							abdicated = true
							break
						}
					}

					if !abdicated {
						c.Logger.Warnf("Abdication failed too many times consecutively (%d), aborting retries", AbdicationMaxAttempts)
						return
					}

					// Abdication succeeded, so we submit the transaction (which forwards to the leader).
					// We do 7 attempts at increasing intervals (1/2/4/8/16/32) or up to EvictionConfigTxForwardingTimeOut.
					timeout := time.After(EvictionConfigTxForwardingTimeOut)
					retryInterval := EvictionConfigTxForwardingTimeOut / 64
					for {
						err := c.Submit(msg, 0)
						if err == nil {
							c.Logger.Warnf("Reconfiguration transaction forwarded successfully")
							break
						}

						c.Logger.Warnf("Reconfiguration transaction forwarding failed, will try again in %s, error: %s", retryInterval, err)

						select {
						case <-time.After(retryInterval):
							retryInterval = 2 * retryInterval
							continue
						case <-timeout:
							c.Logger.Warnf("Reconfiguration transaction forwarding failed with timeout after %s", EvictionConfigTxForwardingTimeOut)
							return
						}
					}
				}()
				return nil, false, nil
			}*/

		batch := c.support.BlockCutter().Cut()
		batches = [][]*common.Envelope{}
		if len(batch) != 0 {
			batches = append(batches, batch)
		}
		batches = append(batches, []*common.Envelope{msg.Payload})
		return batches, false, nil
	}

	c.Logger.Infof("Message Last Validation Sequence: %v", msg.LastValidationSeq)
	c.Logger.Infof("Sequence: %v", seq)
	// it is a normal message
	if msg.LastValidationSeq < seq {
		c.Logger.Warnf("Normal message was validated against %d, although current config seq has advanced (%d)", msg.LastValidationSeq, seq)
		if _, err := c.support.ProcessNormalMsg(msg.Payload); err != nil {
			//c.Metrics.ProposalFailures.Add(1)
			return nil, true, errors.Errorf("bad normal message: %s", err)
		}
	}
	batches, pending = c.support.BlockCutter().Ordered(msg.Payload)
	return batches, pending, nil
}

func (c *Chain) propose(ch chan *common.Block, bc *blockCreator, batches ...[]*common.Envelope) {
	for _, batch := range batches {
		b := bc.createNextBlock(batch)
		c.Logger.Infof("Created block [%d], there are %d blocks in flight", b.Header.Number, c.blockInflight)

		select {
		case ch <- b:
		default:
			c.Logger.Panic("Programming error: limit of in-flight blocks does not properly take effect or block is proposed by follower")
		}

		// if it is config block, then we should wait for the commit of the block
		if protoutil.IsConfigBlock(b) {
			c.configInflight = true
		}

		c.blockInflight++
	}
}

func (c *Chain) writeBlock(block *common.Block, index uint64) {
	if block.Header.Number > c.lastBlock.Header.Number+1 {
		c.Logger.Panicf("Got block [%d], expect block [%d]", block.Header.Number, c.lastBlock.Header.Number+1)
	} else if block.Header.Number < c.lastBlock.Header.Number+1 {
		c.Logger.Infof("Got block [%d], expect block [%d], this node was forced to catch up", block.Header.Number, c.lastBlock.Header.Number+1)
		return
	}

	if c.blockInflight > 0 {
		c.blockInflight-- // only reduce on leader
	}
	c.lastBlock = block

	c.Logger.Infof("Writing block [%d] (BDLS index: %d) to ledger", block.Header.Number, index)

	if protoutil.IsConfigBlock(block) {
		c.configInflight = false
		//c.writeConfigBlock(block, index)
		c.support.WriteConfigBlock(block, nil)
		return
	}

	//c.raftMetadataLock.Lock()
	//c.opts.BlockMetadata.RaftIndex = index
	//m := protoutil.MarshalOrPanic(c.opts.BlockMetadata)
	//c.raftMetadataLock.Unlock()

	c.support.WriteBlock(block, nil)
}

// writeConfigBlock writes configuration blocks into the ledger in
// addition extracts updates about raft replica set and if there
// are changes updates cluster membership as well
/*func (c *Chain) writeConfigBlock(block *common.Block, index uint64) {
	hdr, err := ConfigChannelHeader(block)
	if err != nil {
		c.Logger.Panicf("Failed to get config header type from config block: %s", err)
	}

	c.configInflight = false

	switch common.HeaderType(hdr.Type) {
	case common.HeaderType_CONFIG:
		configMembership := c.detectConfChange(block)

		c.raftMetadataLock.Lock()
		c.opts.BlockMetadata.RaftIndex = index
		if configMembership != nil {
			c.opts.BlockMetadata = configMembership.NewBlockMetadata
			c.opts.Consenters = configMembership.NewConsenters
		}
		c.raftMetadataLock.Unlock()

		blockMetadataBytes := protoutil.MarshalOrPanic(c.opts.BlockMetadata)

		// write block with metadata
		c.support.WriteConfigBlock(block, blockMetadataBytes)

		if configMembership == nil {
			return
		}

		// update membership
		if configMembership.ConfChange != nil {
			// We need to propose conf change in a go routine, because it may be blocked if raft node
			// becomes leaderless, and we should not block `run` so it can keep consuming applyC,
			// otherwise we have a deadlock.
			go func() {
				// ProposeConfChange returns error only if node being stopped.
				// This proposal is dropped by followers because DisableProposalForwarding is enabled.
				if err := c.Node.ProposeConfChange(context.TODO(), *configMembership.ConfChange); err != nil {
					c.Logger.Warnf("Failed to propose configuration update to Raft node: %s", err)
				}
			}()

			c.confChangeInProgress = configMembership.ConfChange

			switch configMembership.ConfChange.Type {
			case raftpb.ConfChangeAddNode:
				c.Logger.Infof("Config block just committed adds node %d, pause accepting transactions till config change is applied", configMembership.ConfChange.NodeID)
			case raftpb.ConfChangeRemoveNode:
				c.Logger.Infof("Config block just committed removes node %d, pause accepting transactions till config change is applied", configMembership.ConfChange.NodeID)
			default:
				c.Logger.Panic("Programming error, encountered unsupported raft config change")
			}

			c.configInflight = true
		} else {
			if err := c.configureComm(); err != nil {
				c.Logger.Panicf("Failed to configure communication: %s", err)
			}
		}

	case common.HeaderType_ORDERER_TRANSACTION:
		c.logger.Panicf("Programming error: unsupported legacy system channel config type: %s", common.HeaderType(hdr.Type))

	default:
		c.logger.Panicf("Programming error: unexpected config type: %s", common.HeaderType(hdr.Type))
	}
}*/

func (c *Chain) configureComm() error {
	// Reset unreachable map when communication is reconfigured
	c.unreachableLock.Lock()
	c.unreachable = make(map[uint64]struct{})
	c.unreachableLock.Unlock()

	nodes, err := c.remotePeers()
	if err != nil {
		return err
	}

	c.configurator.Configure(c.Channel, nodes)
	//c.Comm.Configure(c.support.ChannelID(), nodes)
	return nil
}

func (c *Chain) isConfig(env *common.Envelope) (bool, error) {
	h, err := protoutil.ChannelHeader(env)
	if err != nil {
		c.Logger.Errorf("failed to extract channel header from envelope")
		return false, err
	}

	return h.Type == int32(common.HeaderType_CONFIG), nil
}

func (c *Chain) isRunning() error {
	select {
	case <-c.startC:
	default:
		return errors.Errorf("chain is not started")
	}

	select {
	case <-c.doneC:
		return errors.Errorf("chain is stopped")
	default:
	}

	return nil
}

// Start should allocate whatever resources are needed for staying up to date with the chain.
// Typically, this involves creating a thread which reads from the ordering source, passes those
// messages to a block cutter, and writes the resulting blocks to the ledger.
func (c *Chain) Start() {
	c.Logger.Infof("Starting BDLS node")
	if err := c.configureComm(); err != nil {
		c.Logger.Errorf("Failed to start chain, aborting: +%v", err)
		close(c.doneC)
		return
	}

	close(c.startC)
	close(c.errorC)

	go c.startConsensus(c.config)
	go c.run()
	go c.TestMultiClient()

}

// consensus for one round with full procedure on GRPC cluster
func (c *Chain) startConsensusRPC(config *bdls.Config) error {

	// var propC chan<- *common.Bloc

	// create consensus
	consensus, err := bdls.NewConsensus(config)
	if err != nil {
		c.Logger.Error("cannot create BDLS NewConsensus", err)
	}
	consensus.SetLatency(200 * time.Millisecond)
	// load endpoints
	//var peers []string

	nodes, err := c.remotePeers()
	if err != nil {
		c.Logger.Error("cannot load remotePeers: ", err)
		return err
	}
	for _, node := range nodes {
		if node.NodeAddress.ID == c.bdlsId {
			c.Endpoint = node.NodeAddress.Endpoint

		}
		c.Endpoints = append(c.Endpoints, node.NodeAddress.Endpoint)
		peer := new(Chain)
		consensus.Join(peer)
	}
	c.Node = node{
		endpoint:  c.Endpoint,
		endpoints: c.Endpoints,
	}

	return nil
}

// ############################### BDLS PeerInterface ######################
// fake address for Pipe
type node struct {
	endpoint  string
	endpoints []string
}

func (node) Network() string  { return "tcp" }
func (n node) String() string { return n.endpoint }

// RemoteAddr implements PeerInterface, returns peer's address as connection identity
func (c *Chain) RemoteAddr() net.Addr {
	return c.Node
}

// GetPublicKey returns peer's public key as identity
func (c *Chain) GetPublicKey() *ecdsa.PublicKey {
	return c.consensus.GetPublicKey()
}

// Send implements Peer.Send
func (c *Chain) Send(msg []byte) error {

	return nil
}

// consensus for one round with full procedure
func (c *Chain) startConsensus(config *bdls.Config) error {

	// the consensus updater ticker
	updateTick := time.NewTicker(updatePeriod)

	// create consensus
	consensus, err := bdls.NewConsensus(config)
	if err != nil {
		c.Logger.Error("cannot create BDLS NewConsensus", err)
	}
	consensus.SetLatency(200 * time.Millisecond)
	// load endpoints
	peers := []string{"localhost:4680", "localhost:4681", "localhost:4682", "localhost:4683"}

	// start listener
	tcpaddr, err := net.ResolveTCPAddr("tcp", fmt.Sprint(":", 4679+int(c.bdlsId)))
	if err != nil {
		c.Logger.Error("cannot create ResolveTCPAddr", err)
	}

	l, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		c.Logger.Error("cannot create ListenTCP", err)
	}
	defer l.Close()
	c.Logger.Info("listening on:", fmt.Sprint(":", 4679+int(c.bdlsId)))

	// initiate tcp agent
	transportLayer := agent.NewTCPAgent(consensus, config.PrivateKey)
	if err != nil {
		c.Logger.Error("cannot create NewTCPAgent", err)
	}

	// start updater
	//transportLayer.Update()

	// passive connection from peers
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			c.Logger.Info("peer connected from:", conn.RemoteAddr())
			// peer endpoint created
			p := agent.NewTCPPeer(conn, transportLayer)
			transportLayer.AddPeer(p)
			// prove my identity to this peer
			p.InitiatePublicKeyAuthentication()
		}
	}()

	// active connections to peers
	for k := range peers {
		go func(raddr string) {
			for {
				conn, err := net.Dial("tcp", raddr)
				if err == nil {
					c.Logger.Info("connected to peer:", conn.RemoteAddr())
					// peer endpoint created
					p := agent.NewTCPPeer(conn, transportLayer)
					transportLayer.AddPeer(p)
					// prove my identity to this peer
					p.InitiatePublicKeyAuthentication()
					return
				}
				<-time.After(time.Second)
			}
		}(peers[k])
	}

	c.transportLayer = transportLayer

	for {
		<-updateTick.C
		c.transportLayer.Update()
		height, round, state := c.transportLayer.GetLatestState()
		if height > c.lastBlock.Header.Number {
			// c.Logger.Infof("*** Inside the updateTick and putting data on applyC ***")
			go func() {
				c.applyC <- apply{height: height, round: round, state: state}
			}()
		}
	}

	/*
	   	// c.Order(env, 0)
	   	var bc *blockCreator
	   NEXTHEIGHT:
	   	for {

	   		env := &common.Envelope{
	   			Payload: marshalOrPanic(&common.Payload{
	   				Header: &common.Header{ChannelHeader: marshalOrPanic(&common.ChannelHeader{Type: int32(common.HeaderType_MESSAGE), ChannelId: c.Channel})},
	   				Data:   []byte("TEST_MESSAGE-UNCC-2023-09-12---"),
	   			}),
	   		}
	   		//req := &orderer.SubmitRequest{LastValidationSeq: 0, Payload: env, Channel: c.Channel}
	   		//   batches, pending, err := c.ordered(req)
	   		// if err != nil {
	   		// 	c.Logger.Errorf("Failed to order message: %s", err)
	   		// 	continue
	   		// }
	   		batches, pending := c.support.BlockCutter().Ordered(env)

	   		if !pending && len(batches) == 0 {
	   			c.Logger.Info("batches, pending ", batches, pending)
	   			//continue
	   		}

	   		batch := c.support.BlockCutter().Cut()
	   		if len(batch) == 0 {
	   			c.Logger.Warningf("Batch timer expired with no pending requests, this might indicate a bug")
	   			continue
	   		}

	   		bc = &blockCreator{
	   			hash:   protoutil.BlockHeaderHash(c.lastBlock.Header),
	   			number: c.lastBlock.Header.Number,
	   			logger: c.Logger,
	   		}
	   		//batch = c.support.BlockCutter().Cut()
	   		//for _, batch := range batches {
	   		//block := c.support.CreateNextBlock(batch)

	   		block := bc.createNextBlock(batch)

	   		data, err := proto.Marshal(block)
	   		if err != nil {
	   			c.Logger.Info("*** cannot Marshal Block ", err)
	   		}
	   		//transportLayer.Propose(data)
	   		c.transportLayer.Propose(data)
	   		//} //batches for-loop end
	   		for {
	   			newHeight, _, newState := transportLayer.GetLatestState() //newRound
	   			if newHeight > c.lastBlock.Header.Number {
	   				//h := blake2b.Sum256(data)

	   				newBlock, err := protoutil.UnmarshalBlock(newState)
	   				if err != nil {
	   					return errors.Errorf("failed to unmarshal bdls State to block: %s", err)
	   				}

	   				c.Logger.Infof("Unmarshal bdls State to \r\n block: %v \r\n Header.Number: %v ", newBlock.Data, newBlock.Header.Number)

	   				c.Logger.Info("lastBlock number before write decide block: ", c.lastBlock.Header.Number, c.lastBlock.Header.PreviousHash)

	   				// c.Logger.Infof("<decide> at height:%v round:%v hash:%v", newHeight, newRound, hex.EncodeToString(h[:]))

	   				c.lastBlock = newBlock
	   				// TODO use the bdls <decide> info for the new block

	   				// Using the newState type of bdls.State. it represented the proposed blocked that achived consensus
	   				//c.writeBlock(newBlock, 0)
	   				c.support.WriteBlock(newBlock, nil)

	   				continue NEXTHEIGHT
	   			}
	   			// wait
	   			<-time.After(20 * time.Millisecond)
	   		}
	   		//}
	   	}*/
	return nil
}

func marshalOrPanic(pb proto.Message) []byte {
	data, err := proto.Marshal(pb)
	if err != nil {
		panic(err)
	}
	return data
}

func (c *Chain) apply(height uint64, round uint64, data bdls.State) {
	// c.applyC <- apply{state: data}
	//if app.state != nil {
	c.Logger.Infof("**** Inside apply function *****")
	newBlock, err := protoutil.UnmarshalBlock(data)
	if err != nil {
		c.Logger.Errorf("failed to unmarshal bdls State to block: %s", err)
	}

	// c.Logger.Infof("Unmarshal bdls State to \r\n block data: %v ", newBlock.Data)

	c.Logger.Infof("lastBlock number before write decide block Number : %v ", c.lastBlock.Header.Number)

	// c.Logger.Infof("<decide> at height:%v round:%v hash:%v", newHeight, newRound, hex.EncodeToString(h[:]))

	// c.lastBlock = newBlock

	// Using the newState type of bdls.State.
	// represented the proposed blocked that achived consensus
	//TODO use the c.WriteBlock
	// c.support.WriteBlock(newBlock, nil)

	c.writeBlock(newBlock, 0)

	c.Metrics.CommittedBlockNumber.Set(float64(newBlock.Header.Number))
}

func (c *Chain) run() {

	// timer := time.NewTimer(1 * time.Second)
	ticking := false
	// timer := c.clock.NewTimer(time.Second)
	timer := time.NewTimer(time.Second)

	// we need a stopped timer rather than nil,
	// because we will be select waiting on timer.C()
	if !timer.Stop() {
		<-timer.C
	}

	// if timer is already started, this is a no-op
	startTimer := func() {
		if !ticking {
			ticking = true
			timer.Reset(c.support.SharedConfig().BatchTimeout())
		}
	}

	stopTimer := func() {
		if !timer.Stop() && ticking {
			// we only need to drain the channel if the timer expired (not explicitly stopped)
			<-timer.C
		}
		ticking = false
	}
	//TODO replace the:
	// defer updateTick.Stop()

	submitC := c.submitC
	//var propC chan<- *common.Block

	var bc *blockCreator
	c.Logger.Infof("Start accepting requests  at block [%d]", c.lastBlock.Header.Number)
	// TODO in Propose()
	propC := make(chan *common.Block, c.opts.MaxInflightBlocks) // 5 in-flight blocks in consenter.go

	go func(ch chan *common.Block) {
		for {
			b := <-ch
			// batch := c.support.BlockCutter().Cut()
			// if len(batch) == 0 {
			// 	c.Logger.Warningf("Batch timer expired with no pending requests, this might indicate a bug")
			// 	continue
			// }

			// block := bc.createNextBlock(batch)

			data, err := proto.Marshal(b)
			if err != nil {
				c.Logger.Info("*** INSIDE GOROUTINE cannot Marshal Block ", err)
			}

			// newBlock, err := protoutil.UnmarshalBlock(data)
			// if err != nil {
			// 	c.Logger.Errorf("INSIDE GOROUTINE failed to unmarshal bdls State to block: %s", err)
			// }

			// c.Logger.Infof("INSIDE GOROUTINE Unmarshal bdls State to \r\n block data: %v ", newBlock.Data)

			c.transportLayer.Propose(data)
			c.Logger.Debugf("INSIDE GOROUTINE Proposed block [%d] to bdls consensus", b.Header.Number)
		}
	}(propC)

	// go func() {
	// 	for {
	// 		<-updateTick.C
	// 		// c.Logger.Infof("**** Calling GetLatestState at %v ****", time)
	// 		c.transportLayer.Update()
	// 		height, round, state := c.transportLayer.GetLatestState()
	// 		_ = round // no use
	// 		if height > c.lastBlock.Header.Number {
	// 			// c.apply(height, round, state)
	// 			c.applyC <- apply{state: state}
	// 		}
	// 	}
	// }()

	for {
		select {
		// case <-updateTick.C:
		// 	c.transportLayer.Update()
		// 	height, round, state := c.transportLayer.GetLatestState()
		// 	if height > c.lastBlock.Header.Number {
		// 		c.Logger.Infof("*** Inside the updateTick and putting data on applyC ***")
		// 		go func() {
		// 			c.applyC <- apply{height: height, round: round, state: state}
		// 		}()
		// 	}

		case <-timer.C:
			ticking = false

			c.Logger.Infof("***** Inside Timer *****")
			batch := c.support.BlockCutter().Cut()
			if len(batch) == 0 {
				c.Logger.Warningf("Batch timer expired with no pending requests, this might indicate a bug")
				continue
			}
			c.Logger.Debugf("Batch timer expired, creating block")
			c.Logger.Infof("TTTT Proposing the batch through timer.C TTTT")
			c.propose(propC, bc, batch)

		// case <-updateTick.C:

		// 	//TO Be Use for calling the consensus Update() function
		// 	c.transportLayer.Update()
		// 	//c.Logger.Infof("In <-updateTick.C")
		// 	// check if new block confirmed
		// 	height, round, state := c.transportLayer.GetLatestState()
		// 	if height > c.lastBlock.Header.Number {

		// 		/*newBlock, err := protoutil.UnmarshalBlock(state)
		// 		if err != nil {
		// 			c.Logger.Errorf("failed to unmarshal bdls State to block: %s", err)
		// 		}
		// 		c.lastBlock = newBlock
		// 		c.support.WriteBlock(newBlock, nil)

		// 		c.Logger.Infof("****X**newBlock Number ", newBlock.Header.Number)*/

		// 		go c.apply(height, round, state)

		// 		//c.applyC <- apply{state}
		// 		//return
		// 	}

		// EOL  the consensus updater ticker
		case <-c.chConsensusMessages:
			c.Logger.Infof("**** Getting consensus message *****")
			c.Lock()
			msgs := c.consensusMessages
			c.consensusMessages = nil

			for _, msg := range msgs {
				c.consensus.ReceiveMessage(msg, time.Now())
			}
			c.Unlock()
		case s := <-submitC:
			// countOrder ++

			if s == nil {
				// polled by `WaitReady`
				continue
			}

			bc = &blockCreator{
				hash:   protoutil.BlockHeaderHash(c.lastBlock.Header),
				number: c.lastBlock.Header.Number,
				logger: c.Logger,
			}

			// batches, pending := c.support.BlockCutter().Ordered(s.req.Payload)
			batches, pending, err := c.ordered(s.req)
			if err != nil {
				c.Logger.Errorf("Failed to order message: %s", err)
				continue
			}

			c.Logger.Infof("**** Is Pending Messages: %v ****", pending)

			if !pending && len(batches) == 0 {
				// c.Logger.Info("batches, pending ", batches, pending)
				continue
			}

			if pending {
				startTimer() // no-op if timer is already started
			} else {
				stopTimer()
			}

			// c.Logger.Infof(" ====== Value of countOrder ======: %v", countOrder)
			// if countOrder == 400 {
			// 	batch := c.support.BlockCutter().Cut()
			// 	c.propose(propC, bc, batch)
			// }

			c.Logger.Infof("SSSS Proposing the batches through SubmitC SSSS")

			c.propose(propC, bc, batches...)

			// batch := c.support.BlockCutter().Cut()
			// if len(batch) == 0 {
			// 	c.Logger.Warningf("Batch timer expired with no pending requests, this might indicate a bug")
			// 	continue
			// }

			// block := bc.createNextBlock(batch)
			// data, err := proto.Marshal(block)
			// if err != nil {
			// 	c.Logger.Info("*** cannot Marshal Block ", err)
			// }
			// c.transportLayer.Propose(data)

			// if c.configInflight {
			// 	c.logger.Info("Received config transaction, pause accepting transaction till it is committed")
			// 	// submitC = nil
			// } else
			if c.blockInflight >= c.opts.MaxInflightBlocks {
				c.Logger.Debugf("Number of in-flight blocks (%d) reaches limit (%d), pause accepting transaction",
					c.blockInflight, c.opts.MaxInflightBlocks)
				submitC = nil
			}

		case app := <-c.applyC:
			c.Logger.Infof("**** Inside applyC case ****")
			c.apply(app.height, app.round, app.state)
			if c.blockInflight < c.opts.MaxInflightBlocks {
				submitC = c.submitC
			}

		case <-c.die:
			return

		case <-c.doneC:
			stopTimer()
			//cancelProp()
			// updateTick.Stop()

			select {
			case <-c.errorC: // avoid closing closed channel
			default:
				close(c.errorC)
			}

			c.Logger.Infof("Stop serving requests")
			//c.periodicChecker.Stop()
			return

		}
	}

}

// func (c *Chain) writeBlock(block *common.Block, index uint64) {
// 	if block.Header.Number > c.lastBlock.Header.Number+1 {
// 		c.latency.Panicf("Got block [%d], expect block [%d]", block.Header.Number, c.lastBlock.Header.Number+1)
// 	} else if block.Header.Number < c.lastBlock.Header.Number+1 {
// 		c.Logger.Infof("Got block [%d], expect block [%d], this node was forced to catch up", block.Header.Number, c.lastBlock.Header.Number+1)
// 		return
// 	}

// 	if c.blockInflight > 0 {
// 		c.blockInflight-- // only reduce on leader
// 	}
// 	c.lastBlock = block

// 	c.Logger.Infof("Writing block [%d] (Raft index: %d) to ledger", block.Header.Number, index)

// 	if protoutil.IsConfigBlock(block) {
// 		c.writeConfigBlock(block, index)
// 		return
// 	}

// 	c.raftMetadataLock.Lock()
// 	c.opts.BlockMetadata.RaftIndex = index
// 	m := protoutil.MarshalOrPanic(c.opts.BlockMetadata)
// 	c.raftMetadataLock.Unlock()

// 	c.support.WriteBlock(block, m)
// }
