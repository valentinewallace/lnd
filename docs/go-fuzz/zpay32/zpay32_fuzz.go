package zpay32_fuzz

import (
	"encoding/hex"
	"fmt"
	"github.com/lightningnetwork/lnd/zpay32"
	litecoinCfg "github.com/ltcsuite/ltcd/chaincfg"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/wire"
)

// Fuzz is used by go-fuzz to fuzz for potentially malicious input
func Fuzz(data []byte) int {
	// Because go-fuzz requires this function signature with a []byte parameter,
	// and we want to emulate the behavior of mainScenario in lnwire_test.go,
	// we first parse the []byte parameter into a Message type.

	testPrivKeyBytes, _ := hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), testPrivKeyBytes)
	testMessageSigner := zpay32.MessageSigner{
		SignCompact: func(hash []byte) ([]byte, error) {
			sig, err := btcec.SignCompact(btcec.S256(),
				testPrivKey, hash, true)
			if err != nil {
				return nil, fmt.Errorf("can't sign the "+
					"message: %v", err)
			}
			return sig, nil
		},
	}

	ltcTestNetParams := chaincfg.TestNet3Params
	ltcTestNetParams.Net = wire.BitcoinNet(litecoinCfg.TestNet4Params.Net)
	ltcTestNetParams.Bech32HRPSegwit = litecoinCfg.TestNet4Params.Bech32HRPSegwit
	ltcMainNetParams := chaincfg.MainNetParams
	ltcMainNetParams.Net = wire.BitcoinNet(litecoinCfg.MainNetParams.Net)
	ltcMainNetParams.Bech32HRPSegwit = litecoinCfg.MainNetParams.Bech32HRPSegwit

	// Parsing []byte into Message
	encoded := string(data[:])
	nets := []chaincfg.Params{
		ltcTestNetParams,
		ltcMainNetParams,
		chaincfg.MainNetParams,
		chaincfg.TestNet3Params,
	}
	for _, net := range nets {
		decoded, err := zpay32.Decode(encoded, &net)
		if err == nil {
			reencoded, err := decoded.Encode(testMessageSigner)
			if err != nil {
				panic(err)
			}
			if reencoded != encoded {
				panic(fmt.Errorf("Original encoded invoice and re-encoded invoice are not equal"))
			}

			return 1
		}
	}
	return 0
}
