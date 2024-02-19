/*
Copyright Ahmed Al Salih. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package bdls

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
	c.Logger.Info("TEST TPS start")
	time.Sleep(8 * time.Second)
	multichannel.SetTPSStart()
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go c.TestOrderClient1(wg)

	wg.Wait()

}

func (c *Chain) TestOrderClient1(wg *sync.WaitGroup) {
	c.Logger.Infof("For client %v", 1)
	for i := 0; i < 100000; i++ {
		env := &common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{Type: int32(common.HeaderType_MESSAGE), ChannelId: c.Channel})},
				//Data:   []byte(fmt.Sprintf("100 Byte Asset Size-TEST_MESSAGE-UNCC-Client-1_MESSAGE-TODO-VERIFY-1-Msg number: %v.", i)),
				Data: make([]byte, 1000),
			}),
		}
		c.Order(env, 0)
	}
	wg.Done()
}
