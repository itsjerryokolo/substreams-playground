package exchange

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	eth "github.com/streamingfast/eth-go"
	"github.com/streamingfast/sparkle-pancakeswap/state"
	pbcodec "github.com/streamingfast/sparkle/pb/sf/ethereum/codec/v1"
)

type SwapsExtractor struct {
	*SubstreamIntrinsics
}

// func (p *SwapsExtractor) Map(pairDeltas []state.StateDelta) (out Debeziums, err error) {
// 	for _, delta := range pairDeltas {
// 		newDeb := Debezium{}
// 		if delta.Op == "c" {
// 			newDeb = "CREATE"
// 		}

// 		prevPair := delta.OldValue.DecodeInto(pair)
// 		newPair := delta.NewValue.DecodeInto(pair)
// 		newDeb.SetNewField("transaction_id", newPair.TransactionID)
// 		if oldPair != nil {
// 			newDeb.SetOldField("transaction_id", oldPair.TransactionID)
// 		}
// 	}
// }

func (p *SwapsExtractor) Map(block *pbcodec.Block, pairs state.Reader, prices state.Reader) (out PCSEvents, err error) {
	for _, trx := range block.TransactionTraces {
		trxID := eth.Hash(trx.Hash).Pretty()
		for _, call := range trx.Calls {
			if call.StateReverted {
				continue
			}
			if len(call.Logs) == 0 {
				continue
			}

			pairAddr := eth.Address(call.Address).Pretty()
			pairCnt, found := pairs.GetLast("pair:" + pairAddr)
			if !found {
				continue
			}

			var pair *PCSPair
			if err := json.Unmarshal(pairCnt, &pair); err != nil {
				return nil, err
			}

			var events []interface{}
			for _, log := range call.Logs {
				ethLog := ssCodecLogToEthLog(log)
				event, err := DecodeEvent(ethLog, block, trx)
				if err != nil {
					return nil, fmt.Errorf("parsing event: %w", err)
				}

				events = append(events, event)
			}

			// Match the different patterns
			fmt.Printf("CALL %d on pair %q: ", call.Index, pairAddr)
			for _, ev := range events {
				fmt.Printf("%s ", strings.Replace(strings.Replace(strings.Split(fmt.Sprintf("%T", ev), ".")[1], "Pair", "", -1), "Event", "", -1))
			}
			fmt.Printf("\n")

			_ = pairCnt

			// First pattern:
			// last = Mint, 4 logs (includes the handling of the first optional Transfer)
			// implies: Transfer Transfer Sync Mint
			var newOutput PCSEvent
			var err error
			if len(events) == 4 {
				evMint, okMint := events[3].(*PairMintEvent)
				evBurn, _ := events[3].(*PairBurnEvent)
				evSync := events[2].(*PairSyncEvent)
				evTr2 := events[1].(*PairTransferEvent)
				evTr1 := events[0].(*PairTransferEvent)
				if okMint {
					newOutput, err = p.processMint(prices, pair, evTr1, evTr2, evSync, evMint)
				} else {
					newOutput, err = p.processBurn(prices, pair, evTr1, evTr2, evSync, evBurn)
				}
			} else if len(events) == 3 {
				evMint, okMint := events[2].(*PairMintEvent)
				evBurn, _ := events[2].(*PairBurnEvent)
				evSync := events[1].(*PairSyncEvent)
				evTr2 := events[0].(*PairTransferEvent)
				if okMint {
					newOutput, err = p.processMint(prices, pair, nil, evTr2, evSync, evMint)
				} else {
					newOutput, err = p.processBurn(prices, pair, nil, evTr2, evSync, evBurn)
				}
			} else if len(events) == 2 {
				evSwap, okSwap := events[1].(*PairSwapEvent)
				if okSwap {
					evSync := events[0].(*PairSyncEvent)
					newOutput, err = p.processSwap(prices, pair, evSync, evSwap, eth.Address(trx.From).Pretty())
				} else {
					fmt.Println("HUh? what's that?")
				}
			} else if len(events) == 1 {
				if _, ok := events[0].(*PairTransferEvent); ok {
					//newOutput = p.processTransfer(prices, evTransfer)
				} else if _, ok := events[0].(*PairApprovalEvent); ok {
					//newOutput = p.processApproval(prices, evApproval)
				} else {
					panic("unhandled event pattern, with 1 event")
				}
			} else {
				panic(fmt.Sprintf("unhandled event patttern with %d events", len(events)))
			}
			if err != nil {
				return nil, fmt.Errorf("process pair call: %w", err)
			}
			if newOutput != nil {
				newOutput.SetBasics(pairAddr, pair.Token0.Address, pair.Token1.Address, trxID, uint64(block.MustTime().Unix()))
				out = append(out, newOutput)
			}
		}
	}
	return
}

func (p *SwapsExtractor) processMint(prices state.Reader, pair *PCSPair, tr1 *PairTransferEvent, tr2 *PairTransferEvent, sync *PairSyncEvent, mint *PairMintEvent) (out *PCSMint, err error) {
	logOrdinal := uint64(mint.LogIndex)

	out = &PCSMint{
		LogOrdinal: logOrdinal,
		Liquidity:  tr2.Value.String(),
	}
	return nil, nil
}

func (p *SwapsExtractor) processBurn(prices state.Reader, pair *PCSPair, tr1 *PairTransferEvent, tr2 *PairTransferEvent, sync *PairSyncEvent, burn *PairBurnEvent) (out *PCSBurn, err error) {
	logOrdinal := uint64(burn.LogIndex)
	out = &PCSBurn{
		LogOrdinal: logOrdinal,
		Liquidity: tr2.Value.String(),
	}
	return nil, nil
}

func (p *SwapsExtractor) processSwap(prices state.Reader, pair *PCSPair, sync *PairSyncEvent, swap *PairSwapEvent, fromAddr string) (out *PCSSwap, err error) {
	logOrdinal := uint64(swap.LogIndex)

	amount0In := ConvertTokenToDecimal(swap.Amount0In, pair.Token0.Decimals)
	amount1In := ConvertTokenToDecimal(swap.Amount1In, pair.Token1.Decimals)
	amount0Out := ConvertTokenToDecimal(swap.Amount0Out, pair.Token0.Decimals)
	amount1Out := ConvertTokenToDecimal(swap.Amount1Out, pair.Token1.Decimals)

	amount0Total := bf().Add(amount0Out, amount0In)
	amount1Total := bf().Add(amount1Out, amount1In)

	derivedAmountBNB := avgFloats(
		getDerivedPrice(logOrdinal, prices, "bnb", amount0Total, pair.Token0.Address),
		getDerivedPrice(logOrdinal, prices, "bnb", amount1Total, pair.Token1.Address),
	)

	trackedAmountUSD := avgFloats(
		getDerivedPrice(logOrdinal, prices, "usd", amount0Total, pair.Token0.Address),
		getDerivedPrice(logOrdinal, prices, "usd", amount1Total, pair.Token0.Address),
	)

	out = &PCSSwap{
		LogOrdinal: logOrdinal,

		Amount0In:  amount0In.String(),
		Amount1In:  amount1In.String(),
		Amount0Out: amount0Out.String(),
		Amount1Out: amount1Out.String(),

		AmountBNB: floatToStr(derivedAmountBNB),
		AmountUSD: floatToStr(trackedAmountUSD),
		From:      fromAddr,
		To:        swap.To.Pretty(),
		Sender:    swap.Sender.Pretty(),
	}
	return
}

// func (p *SwapsExtractor) processTransfer(prices state.Reader, tr *PairTransferEvent) (swaps Swaps, err error) {
// 	return nil, nil
// }

// func (p *SwapsExtractor) processApproval(prices state.Reader, approval *PairApprovalEvent) (swaps Swaps, err error) {
// 	return nil, nil
// }

func getDerivedPrice(ord uint64, prices state.Reader, derivedToken string, tokenAmount *big.Float, tokenAddr string) *big.Float {
	usdPrice := foundOrZeroFloat(prices.GetAt(ord, fmt.Sprintf("price:%s:%s", tokenAddr, derivedToken)))
	if usdPrice.Cmp(big.NewFloat(0)) == 0 {
		return nil
	}

	return bf().Mul(tokenAmount, usdPrice)
}

func avgFloats(f ...*big.Float) *big.Float {
	sum := big.NewFloat(0)
	var count float64 = 0
	for _, fl := range f {
		if fl == nil {
			continue
		}
		sum = bf().Add(sum, fl)
		count++
	}

	if count == 0 {
		return sum
	}

	return bf().Quo(sum, big.NewFloat(count))
}
