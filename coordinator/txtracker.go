/*
 * Copyright 2019, Offchain Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package coordinator

import (
	"log"
	"math/big"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/offchainlabs/arb-util/evm"
	"github.com/offchainlabs/arb-util/value"

	solsha3 "github.com/miguelmota/go-solidity-sha3"

	"github.com/offchainlabs/arb-validator/ethvalidator"
	"github.com/offchainlabs/arb-validator/hashing"
	"github.com/offchainlabs/arb-validator/valmessage"
)

type validatorRequest interface {
}

type assertionCountRequest struct {
	resultChan chan<- int
}

type unanVMCreatedEventTxHashRequest struct {
	resultChan chan<- [32]byte
}

type txRequest struct {
	txHash     [32]byte
	resultChan chan<- txInfo
}

type findLogsRequest struct {
	fromHeight *int64
	toHeight   *int64
	address    *big.Int
	topics     [][32]byte

	resultChan chan<- []LogInfo
}

type logsInfo struct {
	msg  evm.EthMsg
	Logs []evm.Log
}

type txInfo struct {
	Found          bool
	assertionIndex int
	RawVal         value.Value
	LogsPreHash    string
	LogsPostHash   string
	LogsValHashes  []string
	ValidatorSigs  []string
	PartialHash    string
	OnChainTxHash  string
}

type assertionInfo struct {
	TxLogs        []logsInfo
	LogsAccHashes []string
	LogsValHashes []string
}

type logResponse struct {
	Log evm.Log
	Msg evm.EthMsg
}

func (a *assertionInfo) FindLogs(address *big.Int, topics [][32]byte) []logResponse {
	logs := make([]logResponse, 0)
	for _, txLogs := range a.TxLogs {
		for _, evmLog := range txLogs.Logs {
			if address != nil && !value.NewIntValue(address).Equal(evmLog.ContractId) {
				continue
			}

			if len(topics) > len(evmLog.Topics) {
				continue
			}

			match := true
			for i, topic := range topics {
				if topic != evmLog.Topics[i] {
					match = false
					break
				}
			}
			if match {
				logs = append(logs, logResponse{evmLog, txLogs.msg})
			}
		}
	}
	return logs
}

func newAssertionInfo() *assertionInfo {
	logs := make([]logsInfo, 0)
	logsAccHashes := make([]string, 0)
	logsValHashes := make([]string, 0)
	return &assertionInfo{logs, logsAccHashes, logsValHashes}
}

type txTracker struct {
	txRequestIndex int
	transactions   map[[32]byte]txInfo
	assertionInfo  []*assertionInfo
	accountNonces  map[common.Address]uint64
	vmID           [32]byte
}

func newTxTracker(vmID [32]byte) *txTracker {
	return &txTracker{
		txRequestIndex: 0,
		transactions:   make(map[[32]byte]txInfo),
		assertionInfo:  make([]*assertionInfo, 0),
		accountNonces:  make(map[common.Address]uint64),
		vmID:           vmID,
	}
}

func (tr *txTracker) processFinalizedAssertion(assertion valmessage.FinalizedAssertion) {
	log.Println("Coordinator produced finalized assertion")
	info := newAssertionInfo()

	if assertion.Assertion != assertion.ProposalResults.Assertion {
		panic("assertion should be the same assertion in ProposalResults")
	}
	partialHashBytes, err := hashing.UnanimousAssertPartialHash(
		tr.vmID,
		assertion.ProposalResults.SequenceNum,
		assertion.ProposalResults.BeforeHash,
		assertion.ProposalResults.TimeBounds,
		assertion.ProposalResults.NewInboxHash,
		assertion.ProposalResults.OriginalInboxHash,
		assertion.ProposalResults.Assertion,
	)
	if err != nil {
		panic("Could not create partial hash")
	}
	partialHash := hexutil.Encode(partialHashBytes[:])

	// TODO: cache calculations (only need to calculate for NewLogs()
	info.LogsValHashes = make([]string, 0, len(assertion.Assertion.Logs))
	info.LogsAccHashes = make([]string, 0, len(assertion.Assertion.Logs))
	var acc []byte
	for _, logsVal := range assertion.Assertion.Logs {
		logsValHash := logsVal.Hash()
		info.LogsValHashes = append(info.LogsValHashes,
			hexutil.Encode(logsValHash[:]))
		acc = solsha3.SoliditySHA3(
			solsha3.Bytes32(acc),
			solsha3.Bytes32(logsVal.Hash()),
		)
		info.LogsAccHashes = append(info.LogsAccHashes,
			hexutil.Encode(acc))
	}

	logsPostHash := info.LogsAccHashes[len(info.LogsAccHashes)-1]
	logsPreHash := hexutil.Encode(solsha3.Bytes32(0))
	for i, res := range assertion.NewLogs() {
		evmVal, err := evm.ProcessLog(res)
		if err != nil {
			log.Printf("VM produced invalid evm result: %v\n", err)
		}

		msg := evmVal.GetEthMsg()
		msgHash := msg.MsgHash(tr.vmID)

		log.Println("Coordinator got response for", hexutil.Encode(msgHash[:]))

		// pre hash index phi can only be < 0 on the first loop
		phi := len(info.LogsAccHashes) - assertion.NewLogCount - (i + 1)
		if phi >= 0 {
			logsPreHash = info.LogsAccHashes[phi]
		} // else logsPreHash is zero (32 bytes)
		logsValHashes := info.LogsValHashes[len(info.LogsValHashes)-
			assertion.NewLogCount-(i-1):]

		// Encode assertion.Signatures as []string
		sigs := make([]string, 0, len(assertion.Signatures))
		for _, sig := range assertion.Signatures {
			sigs = append(sigs, hexutil.Encode(sig))
		}

		txInfo := txInfo{
			Found:          true,
			assertionIndex: 0,
			RawVal:         res,
			LogsPreHash:    logsPreHash,
			LogsPostHash:   logsPostHash,
			LogsValHashes:  logsValHashes,
			ValidatorSigs:  sigs,
			PartialHash:    partialHash,
			OnChainTxHash:  hexutil.Encode(assertion.OnChainTxHash),
		}
		txInfo.assertionIndex = len(tr.assertionInfo)
		switch evmVal := evmVal.(type) {
		case evm.Stop:
			info.TxLogs = append(info.TxLogs, logsInfo{evmVal.Msg, evmVal.Logs})
		case evm.Return:
			info.TxLogs = append(info.TxLogs, logsInfo{evmVal.Msg, evmVal.Logs})
		case evm.Revert:
		}
		tr.transactions[msgHash] = txInfo
	}
	tr.assertionInfo = append(tr.assertionInfo, info)
}

func (tr *txTracker) processRequest(request validatorRequest, val *ethvalidator.EthValidator) {
	switch request := request.(type) {
	case unanVMCreatedEventTxHashRequest:
		request.resultChan <- <-val.VMCreatedEventTxHashChan
	case assertionCountRequest:
		request.resultChan <- len(tr.assertionInfo) - 1
	case txRequest:
		tx, ok := tr.transactions[request.txHash]
		if ok {
			request.resultChan <- tx
		} else {
			request.resultChan <- txInfo{Found: false}
		}
	case findLogsRequest:
		startHeight := int64(0)
		endHeight := int64(len(tr.assertionInfo))
		if request.fromHeight != nil && *request.fromHeight > int64(0) {
			startHeight = *request.fromHeight
		}
		if request.toHeight != nil {
			altEndHeight := *request.toHeight + 1
			if endHeight > altEndHeight {
				endHeight = altEndHeight
			}
		}
		logs := make([]LogInfo, 0)
		if startHeight >= int64(len(tr.assertionInfo)) {
			request.resultChan <- logs
			break
		}
		assertions := tr.assertionInfo[startHeight:endHeight]

		for i, assertion := range assertions {
			assertionLogs := assertion.FindLogs(request.address, request.topics)
			for j, evmLog := range assertionLogs {
				addressBytes := evmLog.Log.ContractId.ToBytes()
				topicStrings := make([]string, 0, len(evmLog.Log.Topics))
				for _, topic := range evmLog.Log.Topics {
					topicStrings = append(topicStrings, hexutil.Encode(topic[:]))
				}
				txHash := evmLog.Msg.MsgHash(tr.vmID)
				logs = append(logs, LogInfo{
					Address:          hexutil.Encode(addressBytes[12:]),
					BlockHash:        hexutil.Encode(txHash[:]),
					BlockNumber:      "0x" + strconv.FormatInt(startHeight+int64(i), 16),
					Data:             hexutil.Encode(evmLog.Log.Data[:]),
					LogIndex:         "0x" + strconv.FormatInt(int64(j), 16),
					Topics:           topicStrings,
					TransactionIndex: "0x0",
					TransactionHash:  hexutil.Encode(txHash[:]),
				})
			}
		}
		request.resultChan <- logs
	}
}

func (tr *txTracker) handleTxResults(
	val *ethvalidator.EthValidator,
	requests chan validatorRequest,
) {
	for {
		select {
		case finalizedAssertion := <-val.CompletedCallChan:
			tr.processFinalizedAssertion(finalizedAssertion)
		case request := <-requests:
			tr.processRequest(request, val)
		}
	}
}
