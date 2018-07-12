package bitcoindnotify

import (
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/integration/rpctest"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcutil"

	// Required to register the boltdb walletdb implementation.
	_ "github.com/roasbeef/btcwallet/walletdb/bdb"
)

var (
	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	netParams       = &chaincfg.RegressionNetParams
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKey)
	addrPk, _       = btcutil.NewAddressPubKey(pubKey.SerializeCompressed(),
		netParams)
	testAddr = addrPk.AddressPubKeyHash()
)

func testCatchUpOnMissedBlocks(miner1 *rpctest.Harness,
	notifier *BitcoindNotifier, t *testing.T) {
	// We'd like to test the case of multiple registered clients receiving
	// historical block epoch notifications due to the notifier's best known
	// block being out of date.
	const numBlocks = 10
	const numClients = 5
	var wg sync.WaitGroup

	_, bestHeight, err := miner1.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// First, generate the blocks that the notifier will need to send
	// historical notifications for.
	_, err = miner1.Node.Generate(numBlocks)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}
	time.Sleep(5 * time.Second)

	// Create numClients clients who will listen for block notifications.
	// We expect each client to receive numBlocks + 1 notifications, 1 for each block
	// that the notifier has missed out on.
	// So we'll use a WaitGroup to synchronize the test.
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			t.Fatalf("unable to register for epoch notification: %v", err)
		}

		wg.Add(numBlocks + 1)
		go func() {
			for i := 0; i < numBlocks+1; i++ {
				<-epochClient.Epochs
				wg.Done()
			}
		}()
	}

	// Reset the notifier's best block to be the block right before
	// we mined, to simulate the notifier missing all the generated blocks.
	if err = notifier.rewindChain(bestHeight); err != nil {
		t.Fatalf("unable to rewind chain: %v", err)
	}

	// Generate a single block to trigger the backlog of historical
	// notifications for the previously mined blocks.
	// Each client should receive numBlocks + 1 notifications, thereby
	// unblocking the goroutine above.
	if _, err := miner1.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	epochsSent := make(chan struct{})
	go func() {
		wg.Wait()
		close(epochsSent)
	}()

	select {
	case <-epochsSent:
	case <-time.After(2000000 * time.Second):
		t.Fatalf("all historical notifications not sent")
	}
}

func testCatchUpOnMissedBlocksWithReorg(miner *rpctest.Harness,
	notifier *BitcoindNotifier, t *testing.T) {
	// We'd like to ensure that a client will still receive all valid
	// block notifications in the case where a notifier's best block has
	// been reorged out of the chain.
	const numBlocks = 10
	const numClients = 5
	var wg sync.WaitGroup

	// Set up a new miner that we can use to cause a reorg.
	miner2, err := rpctest.New(netParams, nil, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	if err := miner2.SetUp(false, 0); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}
	defer miner2.TearDown()

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	if err := rpctest.ConnectNode(miner, miner2); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{miner, miner2}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two should be on the same blockheight.
	_, nodeHeight1, err := miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, nodeHeight2, err := miner2.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if nodeHeight1 != nodeHeight2 {
		t.Fatalf("expected both miners to be on the same height: %v vs %v",
			nodeHeight1, nodeHeight2)
	}

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = miner.Node.AddNode(miner2.P2PAddress(), rpcclient.ANRemove)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Now mine on each chain separately
	blocks, err := miner.Node.Generate(numBlocks)
	if err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	_, err = miner2.Node.Generate(numBlocks)
	if err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	// Each client should receive 1 notification for each of the blocks
	// mined on the longer chain.
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			t.Fatalf("unable to register for epoch notification: %v", err)
		}

		wg.Add(numBlocks + 1)
		go func() {
			for i := 0; i < numBlocks+1; i++ {
				<-epochClient.Epochs
				wg.Done()
			}
		}()
	}

	// We set the notifier's best block to be the last block mined on the
	// shorter chain, to test that the notifier correctly rewinds to
	// the common ancestor between the two chains.
	notifier.bestBlock = chainntnfs.BlockEpoch{
		Height: nodeHeight1 + numBlocks, Hash: blocks[numBlocks-1]}

	// Generate a single block, which should trigger the notifier to rewind
	// to the common ancestor and dispatch notifications from there.
	_, err = miner2.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	epochsSent := make(chan struct{})
	go func() {
		wg.Wait()
		close(epochsSent)
	}()

	select {
	case <-epochsSent:
	case <-time.After(20 * time.Second):
		t.Fatalf("all historical notifications not sent")
	}
}

type testCase struct {
	name string

	test func(node *rpctest.Harness, notifier *BitcoindNotifier, t *testing.T)
}

var ntfnTests = []testCase{
	{
		name: "test catch up on missed blocks w/ reorged best block",
		test: testCatchUpOnMissedBlocksWithReorg,
	},
	{
		name: "test catch up on missed blocks",
		test: testCatchUpOnMissedBlocks,
	},
}

func TestBitcoindNotifier(t *testing.T) {
	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set up
	// this node with a chain length of 125, so we have plenty of BTC to
	// play around with.
	miner, err := rpctest.New(netParams, nil, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer miner.TearDown()
	if err := miner.SetUp(true, 25); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}

	p2pAddr := miner.P2PAddress()

	log.Printf("Running %v BitcoindNotifier tests\n", len(ntfnTests))
	var (
		notifier chainntnfs.ChainNotifier
		cleanUp  func()
	)
	// Start a bitcoind instance.
	tempBitcoindDir, err := ioutil.TempDir("", "bitcoind")
	if err != nil {
		t.Fatalf("Unable to create temp dir: %v", err)
	}
	zmqPath := "ipc:///" + tempBitcoindDir + "/weks.socket"
	cleanUp1 := func() {
		os.RemoveAll(tempBitcoindDir)
	}
	cleanUp = cleanUp1
	rpcPort := rand.Int()%(65536-1024) + 1024
	bitcoind := exec.Command(
		"bitcoind",
		"-datadir="+tempBitcoindDir,
		"-regtest",
		"-connect="+p2pAddr,
		"-txindex",
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6f"+
			"d$507c670e800a95284294edb5773b05544b"+
			"220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		"-disablewallet",
		"-zmqpubrawblock="+zmqPath,
		"-zmqpubrawtx="+zmqPath,
	)
	err = bitcoind.Start()
	if err != nil {
		cleanUp1()
		t.Fatalf("Couldn't start bitcoind: %v", err)
	}
	cleanUp2 := func() {
		bitcoind.Process.Kill()
		bitcoind.Wait()
		cleanUp1()
	}
	cleanUp = cleanUp2

	// Wait for the bitcoind instance to start up.
	time.Sleep(time.Second)

	// Start the FilteredChainView implementation instance.
	config := rpcclient.ConnConfig{
		Host: fmt.Sprintf(
			"127.0.0.1:%d", rpcPort),
		User:                 "weks",
		Pass:                 "weks",
		DisableAutoReconnect: false,
		DisableConnectOnNew:  true,
		DisableTLS:           true,
		HTTPPostMode:         true,
	}

	notifier, err = New(&config, zmqPath,
		*netParams)
	if err != nil {
		t.Fatalf("unable to create bitcoind notifier: %v", err)
	}

	if err := notifier.Start(); err != nil {
		t.Fatalf("unable to start bitcoind notifier: %v",
			err)
	}

	for _, ntfnTest := range ntfnTests {
		testName := fmt.Sprintf("bitcoind: %v",
			ntfnTest.name)

		success := t.Run(testName, func(t *testing.T) {
			ntfnTest.test(miner, notifier.(*BitcoindNotifier), t)
		})

		if !success {
			break
		}
	}

	notifier.Stop()
	if cleanUp != nil {
		cleanUp()
	}
	cleanUp = nil
}
