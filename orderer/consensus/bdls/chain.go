package bdls

import (
	"github.com/BDLS-bft/bdls"
	cb "github.com/hyperledger/fabric-protos-go/common"
)

type Chain struct {
	config *bdls.Config
}

func (c *Chain) Order(env *cb.Envelope, configSeq uint64) error {
	// consensus, err := bdls.NewConsensus(c.config)
	return nil
}

func (c *Chain) Configure(config *cb.Envelope, configSeq uint64) error {
	return nil
}

func (c *Chain) WaitReady() error {
	return nil
}

func (c *Chain) Errored() <-chan struct{} {
	return nil
}

func (c *Chain) Start() {
}

func (c *Chain) Halt() {
}
