/*
Copyright BDLS - CLP. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bdls

import (
	bdls "github.com/BDLS-bft/bdls"
	cb "github.com/hyperledger/fabric-protos-go/common"
)



type Chain struct {
	config *bdls.Config
	channelID string
}


func (c *Chain) Order(env *cb.Envelope, configSeq uint64) error {
	return nil
}

func (c *Chain) Configure(config *cb.Envelope, configSeq uint64) error {
	return nil
}

func (c *Chain) Errored() <-chan struct{} {
	return nil
}
