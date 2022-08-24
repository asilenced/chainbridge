// Copyright 2020 Stafi Protocol
// SPDX-License-Identifier: LGPL-3.0-only

package stafihub

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	errType "github.com/cosmos/cosmos-sdk/types/errors"

	"github.com/ChainSafe/log15"
	"github.com/cosmos/cosmos-sdk/types"
	"github.com/stafihub/rtoken-relay-core/common/core"
	stafihubClient "github.com/stafihub/stafi-hub-relay-sdk/client"
	stafiHubXBridgeTypes "github.com/stafihub/stafihub/x/bridge/types"
	"github.com/stafiprotocol/chainbridge/utils/msg"
)

const (
	msgLimit = 4096
)

var ErrorTerminated = errors.New("terminated")

type writer struct {
	conn    *Connection
	log     log15.Logger
	sysErr  chan<- error
	msgChan chan msg.Message
	stop    <-chan int
}

func NewWriter(conn *Connection, log log15.Logger, sysErr chan<- error, stop <-chan int) *writer {
	return &writer{
		conn:    conn,
		log:     log,
		sysErr:  sysErr,
		msgChan: make(chan msg.Message, msgLimit),
		stop:    stop,
	}
}

func (w *writer) start() error {
	go func() {
		for {
			select {
			case <-w.stop:
				close(w.msgChan)
				w.log.Info("writer stopped")
				return
			case msg := <-w.msgChan:
				result := w.processMessage(msg)
				w.log.Info("processMessage", "result", result)
				if !result {
					w.sysErr <- fmt.Errorf("processMessage failed")
				}
			}
		}
	}()

	return nil
}

func (w *writer) ResolveMessage(m msg.Message) bool {
	w.msgChan <- m
	return true
}

func (w *writer) processMessage(m msg.Message) bool {
	w.log.Info("ResolveMessage", "Name", w.conn.name, "Destination", m.Destination)
	switch m.Type {
	case msg.FungibleTransfer:
		bigAmt := big.NewInt(0).SetBytes(m.Payload[0].([]byte))
		w.log.Debug("amount info", "amount", bigAmt.String())
		//should not have 0x prefix and length must 64
		resourceIdStr := strings.ToLower(m.ResourceId.Hex())
		if len(resourceIdStr) != 64 {
			w.log.Error("resourceId  length  must be 64")
			return false
		}

		depositNonce := m.DepositNonce.Big().Uint64()
		recipient := m.Payload[1].([]byte)
		recipientHexStr := hex.EncodeToString(recipient)
		receiver, err := types.AccAddressFromHex(recipientHexStr)
		if err != nil {
			w.log.Error("accAddressFromHex failed", "err", err)
			return false
		}
		done := core.UseSdkConfigContext(stafihubClient.GetAccountPrefix())
		receiverStr := receiver.String()
		done()

		w.log.Info("ResolveMessage", "nonce", depositNonce, "source",
			m.Source, "resource", resourceIdStr, "receiver", receiverStr, "amount", bigAmt.String())

		proposalDetail, err := w.conn.client.QueryBridgeProposalDetail(uint32(m.Source), depositNonce, resourceIdStr, bigAmt.String(), receiverStr)
		if err != nil {
			if !strings.Contains(err.Error(), "NotFound") {
				w.log.Error("QueryBridgeProposalDetail failed", "err", err)
				return false
			}
		} else {
			if proposalDetail.Proposal.Executed {
				return true
			}
			for _, voter := range proposalDetail.Proposal.Voters {
				if strings.EqualFold(voter, w.conn.Address()) {
					return true
				}
			}
		}

		voteMsg := stafiHubXBridgeTypes.NewMsgVoteProposal(w.conn.Address(), uint32(m.Source), depositNonce, resourceIdStr, types.NewIntFromBigInt(bigAmt), receiverStr)

		err = w.checkAndReSendWithProposal("voteproposal", voteMsg)
		if err != nil {
			w.log.Error("checkAndReSend failed", "err", err)
			return false
		}
		w.log.Info("checkAndResend ok", "recipient", receiverStr)
		return true

	default:
		w.log.Warn("message type unsupported", "type", m.Type)
		return false
	}
}

func (h *writer) checkAndReSendWithProposal(typeStr string, content *stafiHubXBridgeTypes.MsgVoteProposal) error {
	txHashStr, _, err := h.conn.client.SubmitBridgeProposal(content)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), stafiHubXBridgeTypes.ErrAlreadyExecuted.Error()):
			h.log.Info("no need send, already executed", "txHash", txHashStr, "type", typeStr)
			return nil

		case strings.Contains(err.Error(), stafiHubXBridgeTypes.ErrAlreadyVoted.Error()):
			h.log.Info("no need send, already voted", "txHash", txHashStr, "type", typeStr)
			return nil

		// resend case:
		case strings.Contains(err.Error(), errType.ErrWrongSequence.Error()):
			return h.checkAndReSendWithProposal(txHashStr, content)
		}

		return err
	}

	retry := BlockRetryLimit
	var res *types.TxResponse
	for {
		if retry <= 0 {
			h.log.Error("checkAndReSendWithProposal QueryTxByHash, reach retry limit.",
				"tx hash", txHashStr,
				"err", err)
			return fmt.Errorf("checkAndReSendWithProposal QueryTxByHash reach retry limit, tx hash: %s,err: %s", txHashStr, err)
		}

		//check on chain
		res, err = h.conn.client.QueryTxByHash(txHashStr)
		if err != nil || res.Empty() || res.Height == 0 {
			if res != nil {
				h.log.Debug(fmt.Sprintf(
					"checkAndReSendWithProposal QueryTxByHash, tx failed. will query after %f second",
					BlockRetryInterval.Seconds()),
					"tx hash", txHashStr,
					"res.log", res.RawLog,
					"res.code", res.Code)
			} else {
				h.log.Debug(fmt.Sprintf(
					"checkAndReSendWithProposal QueryTxByHash failed. will query after %f second",
					BlockRetryInterval.Seconds()),
					"tx hash", txHashStr,
					"err", err)
			}

			time.Sleep(BlockRetryInterval)
			retry--
			continue
		}

		if res.Code != 0 {
			switch {
			case strings.Contains(res.RawLog, stafiHubXBridgeTypes.ErrAlreadyExecuted.Error()):
				h.log.Info("no need send, already executed", "txHash", txHashStr, "type", typeStr)
				return nil

			case strings.Contains(res.RawLog, stafiHubXBridgeTypes.ErrAlreadyVoted.Error()):
				h.log.Info("no need send, already voted", "txHash", txHashStr, "type", typeStr)
				return nil

			// resend case
			case strings.Contains(res.RawLog, errType.ErrOutOfGas.Error()):
				return h.checkAndReSendWithProposal(txHashStr, content)
			default:
				return fmt.Errorf("tx failed, txHash: %s, rawlog: %s", txHashStr, res.RawLog)
			}
		}

		break
	}

	h.log.Info("checkAndReSendWithProposal success", "txHash", txHashStr, "type", typeStr)
	return nil
}
