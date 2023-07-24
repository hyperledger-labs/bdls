/*
Copyright BDLS. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bdls

import (
	"github.com/BDLS-bft/bdls"
	cb "github.com/hyperledger/fabric-protos-go/common"
)

// Chain represents a BDLS chain.
type Chain struct {
	// TODO
	Config *bdls.Config
}

// Order accepts a message which has been processed at a given configSeq.
// If the configSeq advances, it is the responsibility of the consenter
// to revalidate and potentially discard the message
// The consenter may return an error, indicating the message was not accepted
func (c *Chain) Order(env *cb.Envelope, configSeq uint64) error {
	return nil
}

// Configure accepts a message which reconfigures the channel and will
// trigger an update to the configSeq if committed.  The configuration must have
// been triggered by a ConfigUpdate message. If the config sequence advances,
// it is the responsibility of the consenter to recompute the resulting config,
// discarding the message if the reconfiguration is no longer valid.
// The consenter may return an error, indicating the message was not accepted
func (c *Chain) Configure(config *cb.Envelope, configSeq uint64) error {
	return nil
}

// WaitReady blocks waiting for consenter to be ready for accepting new messages.
// This is useful when consenter needs to temporarily block ingress messages so
// that in-flight messages can be consumed. It could return error if consenter is
// in erroneous states. If this blocking behavior is not desired, consenter could
// simply return nil.
func (c *Chain) WaitReady() error {
	return nil
}

// Errored returns a channel which will close when an error has occurred.
// This is especially useful for the Deliver client, who must terminate waiting
// clients when the consenter is not up to date.
func (c *Chain) Errored() <-chan struct{} {
	return nil
}

// Start should allocate whatever resources are needed for staying up to date with the chain.
// Typically, this involves creating a thread which reads from the ordering source, passes those
// messages to a block cutter, and writes the resulting blocks to the ledger.
func (c *Chain) Start() {}

// Halt frees the resources which were allocated for this Chain.
func (c *Chain) Halt() {}
