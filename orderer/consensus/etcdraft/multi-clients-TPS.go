/*
Copyright Ahmed Al Salih. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package etcdraft

import (
	"sync"
	"time"

	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/orderer/common/multichannel"
	"github.com/hyperledger/fabric/protoutil"
)

// TestMultiClients function runs multiple clients concurrently
// Submits different envelopes to measure the TPS.
func (c *Chain) TestMultiClients() {

	c.logger.Infof(" ------------------------------- c.raftID is: %v  ---------------------", c.raftID)
	time.Sleep(10 * time.Second)
	multichannel.SetTPSStart()
	if c.raftID == 2 {
		c.logger.Info("************* TEST TPS start---")
		// start := time.Now()
		// c.logger.Debugf("TEST TPS start:", start)

		wg := new(sync.WaitGroup)
		wg.Add(1)

		go c.TestOrderClient100(wg)

		wg.Wait()
	}
	/*end := time.Now()

	total := end.Sub(start)
	tps := float64(10000) / (float64(total) * math.Pow(10, -9))
	c.TPS = tps
	c.logger.Infof("**TEST** The total time of execution is: %v with TPS: %f **TEST**", total, tps)*/
}

func (c *Chain) TestOrderClient100(wg *sync.WaitGroup) {
	c.logger.Infof("For client %v", 1)
	for i := 0; i < 100000; i++ {
		env := &common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{Type: int32(common.HeaderType_MESSAGE), ChannelId: c.channelID})},
				//Data:   []byte(fmt.Sprintf("TEST_MESSAGE-UNCC-Client-1-%v", i)),
				Data: make([]byte, 100),
			}),
		}
		c.Order(env, 0)
	}
	wg.Done()
}
