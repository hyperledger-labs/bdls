package bdls

import (
	"fmt"

	bdls "github.com/BDLS-bft/bdls"
)

type Chain struct {
	// Chain Struct
	config *bdls.Config
}

func (c *Chain) Start() {
	consensus, err := bdls.NewConsensus(c.config)
	checkErrNil(err)
	fmt.Println(consensus)

}

func (c *Chain) Order() {

}

func (c *Chain) Configure() {

}

func (c *Chain) WaitReady() {

}

func (c *Chain) Errored() {

}

func (c *Chain) Halt() {

}

func checkErrNil(err error) {
	if err != nil {
		panic(err)
	}
}
