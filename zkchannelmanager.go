package lnd

/*
    struct Receive_return receive_cgo(char* msg, int length, void* p);
	char* send_cgo(void* p);
*/
import "C"
import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"unsafe"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/jinzhu/copier"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/libzkchannels"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zkchanneldb"
)

type zkChannelManager struct {
	Notifier chainntnfs.ChainNotifier

	wg sync.WaitGroup

	isMerchant bool

	// WatchNewZkChannel is to be called once a new zkchannel enters the final
	// funding stage: waiting for on-chain confirmation. This method sends
	// the channel to the ChainArbitrator so it can watch for any on-chain
	// events related to the channel.
	WatchNewZkChannel func(contractcourt.ZkChainWatcherConfig) error

	dbPath string

	// PublishTransaction facilitates the process of broadcasting a
	// transaction to the network.
	PublishTransaction func(*wire.MsgTx, string) error

	// DisconnectMerchant is used at the end of the establish or pay flow, to disconnect from the merchant.
	DisconnectMerchant func(*btcec.PublicKey) error

	// SelfDelay the number of blocks to wait before a closing transaction output to self can be claimed by the broadcaster (zkChannels).
	SelfDelay int16

	// MinFee is the minimum allowed tx fee for closing transactions (zkChannels).
	MinFee int64

	// MaxFee is the maximum allowed tx fee for closing transactions (zkChannels).
	MaxFee int64

	// ValCpfp is the value in satoshis of the child (aka anchor) output in closing transaction (zkChannels).
	ValCpfp int64

	// BalMinCust is the minimum allowed customer balance in satoshis (zkChannels).
	BalMinCust int64

	// BalMinMerch is the minimum allowed merchant balance in satoshis (zkChannels).
	BalMinMerch int64
}

type Total struct {
	Amount int64 `json:"Amount"`
}

type PaySession struct {
	Amount int64 `json:"Amount"`
}

var (
	channelStateKey   = "channelStateKey"
	zkChannelsKey     = "zkChannelsKey"
	channelTokenKey   = "channelTokenKey"
	merchPkKey        = "merchPkKey"
	escrowTxidKey     = "escrowTxidKey"
	escrowPrevoutKey  = "escrowPrevoutKey"
	signedEscrowTxKey = "signedEscrowTxKey"
	txFeeInfoKey      = "txFeeInfoKey"
	custBalKey        = "custBalKey"
	merchBalKey       = "merchBalKey"
	feeCCKey          = "feeCCKey"
	feeMCKey          = "feeMCKey"
	minFeeKey         = "minFeeKey"
	maxFeeKey         = "maxFeeKey"
	pkScriptKey       = "pkScriptKey"
	totalReceivedKey  = "totalReceivedKey"
)

func newZkChannelManager(isZkMerchant bool, zkChainWatcher func(z contractcourt.ZkChainWatcherConfig) error, dbDirPath string, publishTx func(*wire.MsgTx, string) error, disconnectMerchant func(*btcec.PublicKey) error, selfDelay int16, minFee int64, maxFee int64, valCpfp int64, balMinCust int64, balMinMerch int64) *zkChannelManager {
	var dbPath string
	if isZkMerchant {
		dbPath = path.Join(dbDirPath, "zkmerch.db")
	} else {
		dbPath = path.Join(dbDirPath, "zkcust.db")
	}
	return &zkChannelManager{
		WatchNewZkChannel:  zkChainWatcher,
		isMerchant:         isZkMerchant,
		dbPath:             dbPath,
		PublishTransaction: publishTx,
		DisconnectMerchant: disconnectMerchant,
		SelfDelay:          selfDelay,
		MinFee:             minFee,
		MaxFee:             maxFee,
		ValCpfp:            valCpfp,
		BalMinCust:         balMinCust,
		BalMinMerch:        balMinMerch,
	}
}

func (z *zkChannelManager) failEstablishFlow(peer lnpeer.Peer,
	zkChanErr error) {
	zkchLog.Debugf("Failing zkEstablish flow: %v", zkChanErr)

	//Generic error messages: In some cases we might want specific error messages
	//so that they can be handled differently
	msg := lnwire.ErrorData("zkEstablish failed due to internal error")

	errMsg := &lnwire.Error{
		Data: msg,
	}

	zkchLog.Debugf("Sending zkEstablish error to peer (%x): %v",
		peer.IdentityKey().SerializeCompressed(), errMsg)
	if err := peer.SendMessage(false, errMsg); err != nil {
		zkchLog.Errorf("unable to send error message to peer %v", err)
	}
}

func (z *zkChannelManager) failZkPayFlow(peer lnpeer.Peer,
	zkChanErr error) {

	zkchLog.Debugf("Failing zkPay flow: %v", zkChanErr)

	//Generic error messages: In some cases we might want specific error messages
	//so that they can be handled differently
	msg := lnwire.ErrorData("zkPay failed due to internal error")

	errMsg := &lnwire.Error{
		Data: msg,
	}

	zkchLog.Debugf("Sending zkPay error to peer (%x): %v",
		peer.IdentityKey().SerializeCompressed(), errMsg)
	if err := peer.SendMessage(false, errMsg); err != nil {
		zkchLog.Errorf("unable to send error message to peer %v", err)
	}
}

func (z *zkChannelManager) initCustomer() error {
	isMerch, err := DetermineIfMerch()
	if err != nil {
		return fmt.Errorf("could not determine if this is a Customer or Merchant: %v", err)
	}
	if isMerch {
		return fmt.Errorf("Current directory has already been set up with zk merchant DB. " +
			"Delete zkmerch.db and try again to run zklnd as a customer.")
	}

	isCust, err := DetermineIfCust()
	if err != nil {
		return fmt.Errorf("could not determine if this is a Customer or Merchant: %v", err)
	}
	if !isCust {

		zkchLog.Infof("Creating customer zkchannel db")

		err := zkchanneldb.InitDB(z.dbPath)
		if err != nil {
			return err
		}
	}
	return nil
}

func (z *zkChannelManager) initMerchant(merchName, skM, payoutSkM, disputeSkM string) error {
	isCust, err := DetermineIfCust()
	if err != nil {
		return fmt.Errorf("could not determine if this is a Customer or Merchant: %v", err)
	}
	if isCust {
		return fmt.Errorf("Current directory has already been set up with zk customer DB. " +
			"Delete zkcust.db and try again to run zklnd as a merchant.")
	}
	// If there is already a zkmerch.db set up, skip the initialization step
	isMerch, err := DetermineIfMerch()
	if err != nil {
		return fmt.Errorf("could not determine if this is a Customer or Merchant: %v", err)
	}
	if !isMerch {

		zkchLog.Infof("Initializing merchant setup")

		if merchName == "" {
			merchName = "Merchant"
		}

		dbUrl := "redis://127.0.0.1/"

		channelState, err := libzkchannels.ChannelSetup("channel", z.SelfDelay, z.BalMinCust, z.BalMinMerch, z.ValCpfp, false)
		zkchLog.Debugf("libzkchannels.ChannelSetup done")

		channelState, merchState, err := libzkchannels.InitMerchant(dbUrl, channelState, "merch")
		zkchLog.Debugf("libzkchannels.InitMerchant done")

		channelState, merchState, err = libzkchannels.LoadMerchantWallet(merchState, channelState, skM, payoutSkM, disputeSkM)

		// zkDB add merchState & channelState
		zkMerchDB, err := zkchanneldb.SetupMerchDB(z.dbPath)
		if err != nil {
			return err
		}

		// save merchStateBytes in zkMerchDB
		err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
		if err != nil {
			return err
		}

		// save channelStateBytes in zkMerchDB
		err = zkchanneldb.AddMerchField(zkMerchDB, channelState, channelStateKey)
		if err != nil {
			return err
		}

		zkchannels := make(map[string]libzkchannels.ChannelToken)

		err = zkchanneldb.AddMerchField(zkMerchDB, zkchannels, zkChannelsKey)
		if err != nil {
			return err
		}

		// save totalReceived in zkMerchDB.
		// With no channels initially, the total balance starts off at 0
		totalReceived := Total{
			Amount: 0,
		}
		err = zkchanneldb.AddMerchField(zkMerchDB, totalReceived, totalReceivedKey)
		if err != nil {
			return err
		}

		err = zkMerchDB.Close()
		if err != nil {
			return err
		}
		zkchLog.Info("Merchant initialization complete")
		zkchLog.Info("Merchant Public Key:", *merchState.PkM)
	}
	return nil
}

func (z *zkChannelManager) initZkEstablish(inputSats int64, custUtxoTxIdLe string, index uint32, custInputSk string, custStateSk string, custPayoutSk string, changePubKey string, merchPubKey string, zkChannelName string, custBal int64, merchBal int64, feeCC int64, feeMC int64, p lnpeer.Peer) error {

	zkchLog.Debug("Variables going into InitCustomer :=> ", merchPubKey, custBal, merchBal, "cust")

	// If no fee for cust-close or merch-close were passed in, then use the fee estimator to determine them
	if feeCC == 0 {
		// ZKLND-49: Query LND fee estimator to get latest fee
		feeCC = int64(1000)
	}
	if feeMC == 0 {
		// ZKLND-49: Query LND fee estimator to get latest fee
		feeMC = int64(1000)
	}

	txFeeInfo := libzkchannels.TransactionFeeInfo{
		BalMinCust:  z.BalMinCust,
		BalMinMerch: z.BalMinMerch,
		ValCpFp:     z.ValCpfp,
		FeeCC:       feeCC,
		FeeMC:       feeMC,
		MinFee:      z.MinFee,
		MaxFee:      z.MaxFee,
	}

	channelToken, custState, err := libzkchannels.InitCustomer(merchPubKey, custBal, merchBal, txFeeInfo, "cust")
	channelToken, custState, err = libzkchannels.LoadCustomerWallet(custState, channelToken, custStateSk, custPayoutSk)

	if err != nil {
		zkchLog.Error("InitCustomer", err)
		return err
	}

	zkchLog.Debug("Generated channelToken and custState")
	zkchLog.Debugf("%#v", channelToken)

	custPk := fmt.Sprintf("%v", custState.PkC)
	revLock := fmt.Sprintf("%v", custState.RevLock)

	merchPk := fmt.Sprintf("%v", merchPubKey)

	changePkIsHash := true

	// TODO ZKLND-49: Query fee estimator to determine escrowTx fee
	txFee := int64(1000)
	outputSats := custBal + merchBal
	// _, escrowTxid, escrowPrevout, err := libzkchannels.FormEscrowTx(custUtxoTxIdLe, index, custInputSk, inputSats, outputSats, custPk, merchPk, changePubKey, changePkIsHash, txFee)
	// if err != nil {
	// 	zkchLog.Error("FormEscrowTx: ", err)
	// 	return err
	// }

	// TODO: move escrow signing to a later in establish
	signedEscrowTx, _, escrowTxid, escrowPrevout, err := libzkchannels.SignEscrowTx(custUtxoTxIdLe, index, custInputSk, inputSats, outputSats, custPk, merchPk, changePubKey, changePkIsHash, txFee)
	zkchLog.Info("escrow txid => ", escrowTxid)
	zkchLog.Info("signedEscrowTx => ", signedEscrowTx)
	zkchLog.Info("storing new zkchannel variables for:", zkChannelName)

	zkCustDB, err := zkchanneldb.CreateZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, channelToken, channelTokenKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, merchPk, merchPkKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, custState.CustBalance, custBalKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, custState.MerchBalance, merchBalKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, feeCC, feeCCKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, feeMC, feeMCKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, txFeeInfo.MinFee, minFeeKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, txFeeInfo.MaxFee, maxFeeKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, escrowTxid, escrowTxidKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, escrowPrevout, escrowPrevoutKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, signedEscrowTx, signedEscrowTxKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, txFeeInfo, txFeeInfoKey)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	zkchLog.Debug("Saved custState and channelToken")

	// TODO: see if it's necessary to be sending these
	// Convert fields into bytes
	escrowTxidBytes := []byte(escrowTxid)
	custPkBytes := []byte(custPk)
	escrowPrevoutBytes := []byte(escrowPrevout)
	revLockBytes := []byte(revLock)

	custBalBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(custBalBytes, uint64(custState.CustBalance))

	merchBalBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(merchBalBytes, uint64(custState.MerchBalance))

	zkEstablishOpen := lnwire.ZkEstablishOpen{
		EscrowTxid:    escrowTxidBytes,
		CustPk:        custPkBytes,
		EscrowPrevout: escrowPrevoutBytes,
		RevLock:       revLockBytes,
		CustBal:       custBalBytes,
		MerchBal:      merchBalBytes,
	}

	return p.SendMessage(false, &zkEstablishOpen)
}

func (z *zkChannelManager) processZkEstablishOpen(msg *lnwire.ZkEstablishOpen, p lnpeer.Peer) {

	zkchLog.Debug("Just received ZkEstablishOpen")

	// TODO ZKLND-51: Merchant should check ZkEstablishOpen message before proceeding
	// escrowTxid := string(msg.EscrowTxid)
	// custPk := string(msg.CustPk)
	// escrowPrevout := string(msg.EscrowPrevout)
	// revLock := string(msg.RevLock)

	// custBal := int64(binary.LittleEndian.Uint64(msg.CustBal))
	// merchBal := int64(binary.LittleEndian.Uint64(msg.MerchBal))

	// Add variables to zkchannelsdb
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetMerchField(zkMerchDB, channelStateKey, &channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	zkchLog.Debug("channelState MerchPayOutPk => ", *channelState.MerchPayOutPk)
	zkchLog.Debug("channelState MerchDisputePk => ", *channelState.MerchDisputePk)

	merchClosePk := fmt.Sprintf("%v", *merchState.PayoutPk)
	toSelfDelay, err := libzkchannels.GetSelfDelayBE(channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	// Convert fields into bytes
	merchClosePkBytes := []byte(merchClosePk)
	toSelfDelayBytes := []byte(toSelfDelay)
	channelStateBytes, err := json.Marshal(channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkEstablishAccept := lnwire.ZkEstablishAccept{
		ToSelfDelay:   toSelfDelayBytes,
		MerchPayoutPk: merchClosePkBytes,
		ChannelState:  channelStateBytes,
	}
	err = p.SendMessage(false, &zkEstablishAccept)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkEstablishAccept(msg *lnwire.ZkEstablishAccept, p lnpeer.Peer, zkChannelName string) {

	zkchLog.Debugf("Just received ZkEstablishAccept for %v", zkChannelName)

	toSelfDelay := string(msg.ToSelfDelay)
	merchClosePk := string(msg.MerchPayoutPk)

	var channelState libzkchannels.ChannelState
	err := json.Unmarshal(msg.ChannelState, &channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, channelState, channelStateKey)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var merchPk string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, merchPkKey, &merchPk)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var escrowTxid string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, escrowTxidKey, &escrowTxid)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var custBal int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, custBalKey, &custBal)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var merchBal int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, merchBalKey, &merchBal)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var feeCC int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, feeCCKey, &feeCC)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var feeMC int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, feeMCKey, &feeMC)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var minFee int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, minFeeKey, &minFee)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var maxFee int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, maxFeeKey, &maxFee)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var escrowPrevout string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, escrowPrevoutKey, &escrowPrevout)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	custSk := fmt.Sprintf("%v", custState.SkC)
	custPk := fmt.Sprintf("%v", custState.PkC)
	custClosePk := fmt.Sprintf("%v", custState.PayoutPk)

	zkchLog.Debugf("variables going into FormMerchCloseTx: %#v", escrowTxid, custPk, merchPk, merchClosePk, custBal, merchBal, toSelfDelay)
	merchTxPreimage, err := libzkchannels.FormMerchCloseTx(escrowTxid, custPk, merchPk, merchClosePk, custBal, merchBal, feeMC, channelState.ValCpfp, toSelfDelay)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkchLog.Debug("merch TxPreimage => ", merchTxPreimage)

	custSig, err := libzkchannels.CustomerSignMerchCloseTx(custSk, merchTxPreimage)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkchLog.Debug("custSig on merchCloseTx=> ", custSig)

	// Convert variables to bytes before sending

	custBalBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(custBalBytes, uint64(custBal))

	merchBalBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(merchBalBytes, uint64(merchBal))

	feeCCBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(feeCCBytes, uint64(feeCC))

	feeMCBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(feeMCBytes, uint64(feeMC))

	escrowTxidBytes := []byte(escrowTxid)

	escrowPrevoutBytes := []byte(escrowPrevout)

	custPkBytes := []byte(custPk)
	custSigBytes := []byte(custSig)
	custClosePkBytes := []byte(custClosePk)

	revLock := fmt.Sprintf("%v", custState.RevLock)
	revLockBytes := []byte(revLock)

	zkEstablishMCloseSigned := lnwire.ZkEstablishMCloseSigned{
		CustBal:       custBalBytes,
		MerchBal:      merchBalBytes,
		EscrowTxid:    escrowTxidBytes,
		EscrowPrevout: escrowPrevoutBytes,
		CustPk:        custPkBytes,
		CustSig:       custSigBytes,
		CustClosePk:   custClosePkBytes,
		RevLock:       revLockBytes,
		FeeCC:         feeCCBytes,
		FeeMC:         feeMCBytes,
	}
	err = p.SendMessage(false, &zkEstablishMCloseSigned)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkEstablishMCloseSigned(msg *lnwire.ZkEstablishMCloseSigned, p lnpeer.Peer) {

	zkchLog.Debug("Just received MCloseSigned")

	custPk := string(msg.CustPk)
	custBal := int64(binary.LittleEndian.Uint64(msg.CustBal))
	merchBal := int64(binary.LittleEndian.Uint64(msg.MerchBal))
	feeCC := int64(binary.LittleEndian.Uint64(msg.FeeCC))
	feeMC := int64(binary.LittleEndian.Uint64(msg.FeeMC))
	escrowTxid := string(msg.EscrowTxid)
	escrowPrevout := string(msg.EscrowPrevout)
	revLock := string(msg.RevLock)

	// Convert variables received
	custSig := string(msg.CustSig)
	custClosePk := string(msg.CustClosePk)

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetMerchField(zkMerchDB, channelStateKey, &channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	toSelfDelay, err := libzkchannels.GetSelfDelayBE(channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	isOk, merchTxid_BE, merchTxid, merchPrevout, merchState, err := libzkchannels.MerchantVerifyMerchCloseTx(escrowTxid, custPk, custBal, merchBal, feeMC, channelState.ValCpfp, toSelfDelay, custSig, merchState)
	zkchLog.Infof("isOk?: %v", isOk)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	switch isOk {
	case true:
		zkchLog.Info("MerchantVerifyMerchCloseTx succeeded")
	case false:
		zkchLog.Info("MerchantVerifyMerchCloseTx failed")
	}

	zkchLog.Info("Merch close txid = ", merchTxid)
	zkchLog.Debug("merch prevout = ", merchPrevout)

	// TEMPORARY CODE TO FLIP BYTES
	// This works because hex strings are of even size
	s := ""
	for i := 0; i < len(escrowTxid)/2; i++ {
		s = escrowTxid[i*2:i*2+2] + s
	}
	escrowTxid_BE := s

	// MERCH SIGN CUST CLOSE
	txInfo := libzkchannels.FundingTxInfo{
		EscrowTxId:    escrowTxid_BE,
		EscrowPrevout: escrowPrevout,
		MerchTxId:     merchTxid_BE,
		MerchPrevout:  merchPrevout,
		InitCustBal:   custBal,
		InitMerchBal:  merchBal,
	}

	zkchLog.Debug("RevLock => ", revLock)

	escrowSig, merchSig, err := libzkchannels.MerchantSignInitCustCloseTx(txInfo, revLock, custPk, custClosePk, toSelfDelay, merchState, feeCC, feeMC, channelState.ValCpfp)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	// assert.Nil(t, err)
	zkchLog.Debug("escrow sig: ", escrowSig)
	zkchLog.Debug("merch sig: ", merchSig)

	// Convert variables to bytes before sending
	escrowSigBytes := []byte(escrowSig)
	merchSigBytes := []byte(merchSig)
	merchTxidBytes := []byte(merchTxid)
	merchPrevoutBytes := []byte(merchPrevout)

	zkEstablishCCloseSigned := lnwire.ZkEstablishCCloseSigned{
		EscrowSig:    escrowSigBytes,
		MerchSig:     merchSigBytes,
		MerchTxid:    merchTxidBytes,
		MerchPrevout: merchPrevoutBytes,
	}

	err = p.SendMessage(false, &zkEstablishCCloseSigned)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	// Create and save pkScript and escrowTxid
	// Note that we are reversing the bytes to correct escrowTxid into little endian

	merchPk := fmt.Sprintf("%v", *merchState.PkM)
	multisigScriptHex := []byte("5221" + merchPk + "21" + custPk + "52ae")
	multisigScript := make([]byte, hex.DecodedLen(len(multisigScriptHex)))
	_, err = hex.Decode(multisigScript, multisigScriptHex)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	h := sha256.New()
	h.Write(multisigScript)

	scriptSha := fmt.Sprintf("%x", h.Sum(nil))

	pkScriptHex := []byte("0020" + scriptSha)

	pkScript := make([]byte, hex.DecodedLen(len(pkScriptHex)))
	_, err = hex.Decode(pkScript, pkScriptHex)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkchLog.Debugf("multisigScript: %#x\n", multisigScript)
	zkchLog.Debugf("pkScript: %#x\n", pkScript)

	// ZKLND-11 Merchant support for multiple channels
	// cannot use this method for storing escrowTxid as it will get
	// overwritten by new channels
	err = zkchanneldb.AddMerchField(zkMerchDB, escrowTxid, escrowTxidKey)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchField(zkMerchDB, pkScript, pkScriptKey)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}
}

func (z *zkChannelManager) processZkEstablishCCloseSigned(msg *lnwire.ZkEstablishCCloseSigned, p lnpeer.Peer, zkChannelName string) {

	zkchLog.Debugf("Just received CCloseSigned for %v", zkChannelName)

	// Convert variables received
	escrowSig := string(msg.EscrowSig)
	merchSig := string(msg.MerchSig)
	merchTxid := string(msg.MerchTxid)
	merchPrevout := string(msg.MerchPrevout)

	zkchLog.Debug("escrow sig: ", escrowSig)
	zkchLog.Debug("merch sig: ", merchSig)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var merchPk string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, merchPkKey, &merchPk)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	var escrowTxid string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, escrowTxidKey, &escrowTxid)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var escrowPrevout string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, escrowPrevoutKey, &escrowPrevout)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelStateKey, &channelState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var channelToken libzkchannels.ChannelToken
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelTokenKey, &channelToken)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var custBal int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, custBalKey, &custBal)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var merchBal int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, merchBalKey, &merchBal)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var txFeeInfo libzkchannels.TransactionFeeInfo
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, txFeeInfoKey, &txFeeInfo)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	// TEMPORARY CODE TO FLIP BYTES
	// This works because hex strings are of even size
	s := ""
	for i := 0; i < len(escrowTxid)/2; i++ {
		s = escrowTxid[i*2:i*2+2] + s
	}
	escrowTxid_BE := s

	// TEMPORARY CODE TO FLIP BYTES
	// This works because hex strings are of even size
	s2 := ""
	for i := 0; i < len(merchTxid)/2; i++ {
		s2 = merchTxid[i*2:i*2+2] + s2
	}
	merchTxid_BE := s2

	txInfo := libzkchannels.FundingTxInfo{
		EscrowTxId:    escrowTxid_BE,
		EscrowPrevout: escrowPrevout,
		MerchTxId:     merchTxid_BE,
		MerchPrevout:  merchPrevout,
		InitCustBal:   custBal,
		InitMerchBal:  merchBal,
	}

	isOk, channelToken, custState, err := libzkchannels.CustomerVerifyInitCustCloseTx(txInfo, txFeeInfo, channelState, channelToken, escrowSig, merchSig, custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	switch isOk {
	case true:
		zkchLog.Info("Merch signature on Cust Close is valid")
	case false:
		zkchLog.Info("Merch signature on Cust Close is invalid")
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, channelToken, channelTokenKey)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	initCustState, initHash, err := libzkchannels.CustomerGetInitialState(custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	initCustStateBytes, err := json.Marshal(initCustState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	initHashBytes := []byte(initHash)

	channelTokenBytes, err := json.Marshal(channelToken)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	zkEstablishInitialState := lnwire.ZkEstablishInitialState{
		ChannelToken:  channelTokenBytes,
		InitCustState: initCustStateBytes,
		InitHash:      initHashBytes,
	}

	err = p.SendMessage(false, &zkEstablishInitialState)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkEstablishInitialState(msg *lnwire.ZkEstablishInitialState, p lnpeer.Peer, notifier chainntnfs.ChainNotifier) {

	zkchLog.Info("Just received InitialState")

	var channelToken libzkchannels.ChannelToken
	err := json.Unmarshal(msg.ChannelToken, &channelToken)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var initCustState libzkchannels.InitCustState
	err = json.Unmarshal(msg.InitCustState, &initCustState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	initHash := string(msg.InitHash)

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var escrowTxid string
	err = zkchanneldb.GetMerchField(zkMerchDB, escrowTxidKey, &escrowTxid)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var pkScript []byte
	err = zkchanneldb.GetMerchField(zkMerchDB, pkScriptKey, &pkScript)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	isOk, merchState, err := libzkchannels.MerchantValidateInitialState(channelToken, initCustState, initHash, merchState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	switch isOk {
	case true:
		zkchLog.Info("Customer's initial state is valid")
	case false:
		zkchLog.Info("Customer's initial state is invalid")
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkchannels := make(map[string]libzkchannels.ChannelToken)

	channelID, err := libzkchannels.GetChannelId(channelToken)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	zkchLog.Debugf("ChannelID: %v", channelID)
	zkchannels[channelID] = channelToken
	err = zkchanneldb.AddMerchField(zkMerchDB, zkchannels, zkChannelsKey)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	var successMsg string

	switch isOk {
	case true:
		successMsg = "Initial State Validation Successful"
	case false:
		successMsg = "Initial State Validation Unsuccessful"
	}

	zkEstablishStateValidated := lnwire.ZkEstablishStateValidated{
		SuccessMsg: []byte(successMsg),
	}
	err = p.SendMessage(false, &zkEstablishStateValidated)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	var escrowTxidHash chainhash.Hash
	err = chainhash.Decode(&escrowTxidHash, escrowTxid)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	zkchLog.Debugf("escrowTxidHash: %v", escrowTxidHash.String())

	fundingOut := &wire.OutPoint{
		Hash:  escrowTxidHash,
		Index: uint32(0),
	}
	zkchLog.Debugf("fundingOut: %v", fundingOut)

	ZkFundingInfo := contractcourt.ZkFundingInfo{
		FundingOut:      *fundingOut,
		PkScript:        pkScript,
		BroadcastHeight: uint32(300), // TODO ZKLND-50: Replace with actual fundingtx confirm height
	}
	zkchLog.Debugf("ZkFundingInfo: %v", ZkFundingInfo)
	zkchLog.Debugf("pkScript: %v", ZkFundingInfo.PkScript)

	zkChainWatcherCfg := contractcourt.ZkChainWatcherConfig{
		ZkFundingInfo:   ZkFundingInfo,
		IsMerch:         true,
		CustChannelName: "",
		Notifier:        notifier,
	}
	zkchLog.Debugf("notifier: %v", notifier)

	if err := z.WatchNewZkChannel(zkChainWatcherCfg); err != nil {
		zkchLog.Errorf("Unable to send new ChannelPoint(%v) for "+
			"arbitration: %v", escrowTxid, err)
	}

	// Wait for on chain confirmations of escrow transaction
	z.wg.Add(1)
	go z.advanceMerchantStateAfterConfirmations(notifier, true, escrowTxid, "", pkScript)

}

func (z *zkChannelManager) advanceMerchantStateAfterConfirmations(notifier chainntnfs.ChainNotifier, confirmOpen bool, escrowTxid string, closeTxid string, pkScript []byte) {

	zkchLog.Debugf("waitForFundingWithTimeout\npkScript: %#x\n", pkScript)

	var txid string
	if confirmOpen == true {
		txid = escrowTxid
	} else {
		txid = closeTxid
	}

	confChannel, err := z.waitForFundingWithTimeout(notifier, txid, pkScript)
	if err != nil {
		zkchLog.Infof("error waiting for funding "+
			"confirmation: %v", err)
	}

	zkchLog.Debugf("confChannel: %#v\n", confChannel)
	zkchLog.Infof("Transaction %v has 3 confirmations", txid)

	// Now that the tx has been confirmed, update the status of the channel in
	// the db
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	if confirmOpen == true {
		merchState, err = libzkchannels.MerchantChangeChannelStatusToOpen(escrowTxid, merchState)
		if err != nil {
			zkchLog.Error(err)
			return
		}
	} else {
		merchState, err = libzkchannels.MerchantChangeChannelStatusToConfirmedClose(escrowTxid, merchState)
		if err != nil {
			zkchLog.Error(err)
			return
		}
	}
	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

}

func (z *zkChannelManager) processZkEstablishStateValidated(msg *lnwire.ZkEstablishStateValidated, p lnpeer.Peer, zkChannelName string, notifier chainntnfs.ChainNotifier) {

	zkchLog.Debugf("Just received ZkEstablishStateValidated for %v", zkChannelName)

	// TODO: For now, we assume isOk is true
	// Add alternative path for when isOk is false

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	var signedEscrowTx string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, signedEscrowTxKey, &signedEscrowTx)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	var escrowTxid string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, escrowTxidKey, &escrowTxid)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	// Convert escrow to wire.MsgTx to broadcast on chain
	serializedTx, err := hex.DecodeString(signedEscrowTx)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	var msgTx wire.MsgTx
	err = msgTx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		zkchLog.Error(err)
		return
	}

	fundingOut := &wire.OutPoint{
		Hash:  msgTx.TxHash(),
		Index: uint32(0),
	}
	zkchLog.Debugf("fundingOut: %v", fundingOut)

	ZkFundingInfo := contractcourt.ZkFundingInfo{
		FundingOut:      *fundingOut,
		PkScript:        msgTx.TxOut[0].PkScript,
		BroadcastHeight: uint32(300), // TODO ZKLND-50: Replace with actual fundingtx confirm height
	}
	zkchLog.Debugf("ZkFundingInfo: %v", ZkFundingInfo)
	zkchLog.Debugf("pkScript: %v", ZkFundingInfo.PkScript)

	zkChainWatcherCfg := contractcourt.ZkChainWatcherConfig{
		ZkFundingInfo:   ZkFundingInfo,
		IsMerch:         false,
		CustChannelName: zkChannelName,
		Notifier:        notifier,
	}
	zkchLog.Debugf("notifier: %v", notifier)

	if err := z.WatchNewZkChannel(zkChainWatcherCfg); err != nil {
		zkchLog.Errorf("Unable to send new ChannelPoint(%v) for "+
			"arbitration: %v", escrowTxid, err)
	}

	zkchLog.Debugf("Broadcasting signedEscrowTx: %#v\n", signedEscrowTx)

	err = z.PublishTransaction(&msgTx, "")
	if err != nil {
		zkchLog.Error(err)
		return
	}

	pkScript := msgTx.TxOut[0].PkScript

	z.wg.Add(1)
	go z.advanceCustomerStateAfterConfirmations(notifier, escrowTxid, pkScript, zkChannelName, p)

}

func (z *zkChannelManager) advanceCustomerStateAfterConfirmations(notifier chainntnfs.ChainNotifier, escrowTxid string, pkScript []byte, zkChannelName string, p lnpeer.Peer) {

	// Wait for confirmations
	confChannel, err := z.waitForFundingWithTimeout(notifier, escrowTxid, pkScript)
	if err != nil {
		zkchLog.Infof("error waiting for funding "+
			"confirmation: %v", err)
	}

	zkchLog.Debugf("%#v\n", confChannel)

	// TEMPORARY DUMMY MESSAGE
	fundingLockedBytes := []byte("Funding Locked")
	zkEstablishFundingLocked := lnwire.ZkEstablishFundingLocked{
		FundingLocked: fundingLockedBytes,
	}

	// Add a flag to zkchannelsdb to say that closeChannel has not been initiated.
	// This is used to prevent another payment being made
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return
	}
	err = zkchanneldb.AddField(zkCustDB, zkChannelName, false, "closeInitiatedKey")
	if err != nil {
		zkchLog.Error(err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	custState, err = libzkchannels.CustomerChangeChannelStatusToOpen(custState)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	err = p.SendMessage(false, &zkEstablishFundingLocked)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	zkchLog.Debugf("confChannel: %#v\n", confChannel)
	zkchLog.Infof("Transaction %v has 3 confirmations", escrowTxid)
}

// waitForFundingWithTimeout is a wrapper around waitForFundingConfirmation and
// waitForTimeout that will return ErrConfirmationTimeout if we are not the
// channel initiator and the maxWaitNumBlocksFundingConf has passed from the
// funding broadcast height. In case of confirmation, the short channel ID of
// // the channel and the funding transaction will be returned.
// func (z *zkChannelManager) waitForFundingWithTimeout(
// 	ch *channeldb.OpenChannel) (*confirmedChannel, error) {

func (z *zkChannelManager) waitForFundingWithTimeout(notifier chainntnfs.ChainNotifier, escrowTxid string, pkScript []byte) (*confirmedChannel, error) {

	confChan := make(chan *confirmedChannel)
	timeoutChan := make(chan error, 1)
	cancelChan := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)

	go z.waitForFundingConfirmation(notifier, cancelChan, confChan, &wg, escrowTxid, pkScript)

	// If we are not the initiator, we have no money at stake and will
	// timeout waiting for the funding transaction to confirm after a
	// while.
	IsInitiator := true
	if !IsInitiator {
		wg.Add(1)
		go z.waitForTimeout(notifier, cancelChan, timeoutChan, &wg)
	}

	defer close(cancelChan)

	select {
	case err := <-timeoutChan:

		if err != nil {
			return nil, err
		}
		return nil, ErrConfirmationTimeout

	// case <-z.quit:
	// 	// The fundingManager is shutting down, and will resume wait on
	// 	// startup.
	// 	return nil, ErrFundingManagerShuttingDown

	case confirmedChannel, ok := <-confChan:
		zkchLog.Info("waitForFundingConfirmation: confirmedChannel")

		if !ok {
			return nil, fmt.Errorf("waiting for funding" +
				"confirmation failed")
		}
		return confirmedChannel, nil
	}
}

// waitForFundingConfirmation handles the final stages of the channel funding
// process once the funding transaction has been broadcast. The primary
// function of waitForFundingConfirmation is to wait for blockchain
// confirmation, and then to notify the other systems that must be notified
// when a channel has become active for lightning transactions.
// The wait can be canceled by closing the cancelChan. In case of success,
// a *lnwire.ShortChannelID will be passed to confChan.
//
// NOTE: This MUST be run as a goroutine.
func (z *zkChannelManager) waitForFundingConfirmation(notifier chainntnfs.ChainNotifier,
	cancelChan <-chan struct{},
	confChan chan<- *confirmedChannel, wg *sync.WaitGroup, escrowTxid string, pkScript []byte) {

	defer wg.Done()
	defer close(confChan)

	// // Register with the ChainNotifier for a notification once the funding
	// // transaction reaches `numConfs` confirmations.
	// fundingScript, err := makeFundingScript(completeChan)
	// if err != nil {
	// 	fndgLog.Errorf("unable to create funding script for "+
	// 		"ChannelPoint(%v): %v", completeChan.FundingOutpoint,
	// 		err)
	// 	return
	// }

	// Print escrowTxid and pkScript
	zkchLog.Infof("Waiting for confirmations for txid: %v with pkScript %x", escrowTxid, pkScript)

	var txid chainhash.Hash
	err := chainhash.Decode(&txid, escrowTxid)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	NumConfsRequired := 3
	numConfs := uint32(NumConfsRequired)
	FundingBroadcastHeight := uint32(420)

	confNtfn, err := notifier.RegisterConfirmationsNtfn(
		&txid, pkScript, numConfs,
		FundingBroadcastHeight,
	)

	if err != nil {
		zkchLog.Errorf("Unable to register for confirmation of "+
			"ChannelPoint", err)
		return
	}

	zkchLog.Infof("Waiting for tx (%v) to reach %v confirmations",
		txid, numConfs)

	var confDetails *chainntnfs.TxConfirmation
	var ok bool

	// Wait until the specified number of confirmations has been reached,
	// we get a cancel signal, or the wallet signals a shutdown.
	select {
	case confDetails, ok = <-confNtfn.Confirmed:
		// fallthrough

	case <-cancelChan:
		zkchLog.Warnf("canceled waiting for funding confirmation, " +
			"stopping funding flow for ChannelPoint")
		return

		// case <-z.quit:
		// 	zkchLog.Warnf("fundingManager shutting down, stopping funding "+
		// 		"flow for ChannelPoint(%v)")
		// return
	}

	if !ok {
		zkchLog.Warnf("ChainNotifier shutting down, cannot complete " +
			"funding flow for ChannelPoint")
		return
	}

	// fundingPoint := completeChan.FundingOutpoint
	// fndgLog.Infof("ChannelPoint(%v) is now active: ChannelID(%v)",
	// 	fundingPoint, lnwire.NewChanIDFromOutPoint(&fundingPoint))

	// // With the block height and the transaction index known, we can
	// // construct the compact chanID which is used on the network to unique
	// // identify channels.
	Index := 0
	shortChanID := lnwire.ShortChannelID{
		BlockHeight: confDetails.BlockHeight,
		TxIndex:     confDetails.TxIndex,
		TxPosition:  uint16(Index),
	}

	select {
	case confChan <- &confirmedChannel{
		shortChanID: shortChanID,
		fundingTx:   confDetails.Tx,
	}:
		// case <-z.quit:
		// return
	}
}

// waitForTimeout will close the timeout channel if maxWaitNumBlocksFundingConf
// has passed from the broadcast height of the given channel. In case of error,
// the error is sent on timeoutChan. The wait can be canceled by closing the
// cancelChan.
//
// NOTE: timeoutChan MUST be buffered.
// NOTE: This MUST be run as a goroutine.
func (z *zkChannelManager) waitForTimeout(notifier chainntnfs.ChainNotifier,
	cancelChan <-chan struct{}, timeoutChan chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		timeoutChan <- fmt.Errorf("unable to register for epoch "+
			"notification: %v", err)
		return
	}

	defer epochClient.Cancel()

	// // On block maxHeight we will cancel the funding confirmation wait.
	// maxHeight := completeChan.FundingBroadcastHeight + maxWaitNumBlocksFundingConf
	maxHeight := uint32(10)
	for {
		select {
		case epoch, ok := <-epochClient.Epochs:
			if !ok {
				timeoutChan <- fmt.Errorf("epoch client " +
					"shutting down")
				return
			}

			// Close the timeout channel and exit if the block is
			// aboce the max height.
			if uint32(epoch.Height) >= maxHeight {
				zkchLog.Warnf("Waited for %v blocks without "+
					"seeing funding transaction confirmed,"+
					" cancelling.",
					maxWaitNumBlocksFundingConf)

				// Notify the caller of the timeout.
				close(timeoutChan)
				return
			}

			// TODO: If we are the channel initiator implement
			// a method for recovering the funds from the funding
			// transaction

		case <-cancelChan:
			return

			// case <-z.quit:
			// 	// The fundingManager is shutting down, will resume
			// 	// waiting for the funding transaction on startup.
			// 	return
		}
	}
}

func (z *zkChannelManager) processZkEstablishFundingLocked(msg *lnwire.ZkEstablishFundingLocked, p lnpeer.Peer) {

	zkchLog.Debug("Just received FundingLocked: ", msg.FundingLocked)

	// TODO: Check (local) channel status has gone from pending to confirmed.
	// Use same channel state from advanceStateAfterConfirmations.

	// TEMPORARY DUMMY MESSAGE
	fundingConfirmedBytes := []byte("Funding Confirmed")
	zkEstablishFundingConfirmed := lnwire.ZkEstablishFundingConfirmed{
		FundingConfirmed: fundingConfirmedBytes,
	}
	err := p.SendMessage(false, &zkEstablishFundingConfirmed)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkEstablishFundingConfirmed(msg *lnwire.ZkEstablishFundingConfirmed, p lnpeer.Peer, zkChannelName string) {

	zkchLog.Debugf("Just received FundingConfirmed for %v", zkChannelName)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	var channelToken libzkchannels.ChannelToken
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelTokenKey, &channelToken)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	zkchLog.Debug("ActivateCustomer, channelToken =>:", channelToken)

	state, custState, err := libzkchannels.ActivateCustomer(custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	zkchLog.Debug("ActivateCustomer, state =>:", state)

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, state, "stateKey")
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	channelTokenBytes, err := json.Marshal(channelToken)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	stateBytes, err := json.Marshal(state)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	zkEstablishCustActivated := lnwire.ZkEstablishCustActivated{
		State:        stateBytes,
		ChannelToken: channelTokenBytes,
	}
	err = p.SendMessage(false, &zkEstablishCustActivated)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkEstablishCustActivated(msg *lnwire.ZkEstablishCustActivated, p lnpeer.Peer) {

	// To load from rpc message
	var state libzkchannels.State
	err := json.Unmarshal(msg.State, &state)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	zkchLog.Debug("Just received ActivateCustomer, state =>:", state)

	var channelToken libzkchannels.ChannelToken
	err = json.Unmarshal(msg.ChannelToken, &channelToken)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}
	zkchLog.Debug("Just received ActivateCustomer, channelToken =>:", channelToken)

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	payToken0, merchState, err := libzkchannels.ActivateMerchant(channelToken, state, merchState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	// TEMPORARY DUMMY MESSAGE
	payToken0Bytes := []byte(payToken0)
	zkEstablishPayToken := lnwire.ZkEstablishPayToken{
		PayToken0: payToken0Bytes,
	}
	err = p.SendMessage(false, &zkEstablishPayToken)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkEstablishPayToken(msg *lnwire.ZkEstablishPayToken, p lnpeer.Peer, zkChannelName string) {

	payToken0 := string(msg.PayToken0)
	zkchLog.Debugf("Just received PayToken0 for %v: ", zkChannelName, payToken0)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	custState, err = libzkchannels.ActivateCustomerFinalize(payToken0, custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	// Now that the pay session has ended, disconnect from the merchant so that
	// subsequent payments can be made on a fresh connection with a new
	// customer nodeID
	err = z.DisconnectMerchant(p.IdentityKey())
	if err != nil {
		zkchLog.Error(err)
	}
}

func (z *zkChannelManager) InitZkPay(p lnpeer.Peer, zkChannelName string, amount int64) error {

	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelStateKey, &channelState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	zkchLog.Debug("channelState MerchPayOutPk => ", *channelState.MerchPayOutPk)
	zkchLog.Debug("channelState MerchDisputePk => ", *channelState.MerchDisputePk)

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	var oldState libzkchannels.State
	err = copier.Copy(&oldState, *custState.State)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	revState, newState, revLockCom, sessionID, custState, err := libzkchannels.PreparePaymentCustomer(channelState, amount, custState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}
	zkchLog.Info("New session ID:", sessionID)

	// TODO: Add sessionID to custDB
	// Add variables to zkchannelsdb
	zkCustDB, err = zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, revState, "revStateKey")
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, revLockCom, "revLockComKey")
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, newState, "newStateKey")
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, oldState, "oldStateKey")
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkchanneldb.AddField(zkCustDB, zkChannelName, amount, "amountKey")
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	sessionIDBytes := []byte(sessionID)
	justification := []byte("")
	oldStateNonce := oldState.Nonce
	oldStateNonceBytes := []byte(oldStateNonce)

	amountBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(amountBytes, uint64(amount))

	revLockComBytes := []byte(revLockCom)

	zkpaynonce := lnwire.ZkPayNonce{
		SessionID:     sessionIDBytes,
		Justification: justification,
		StateNonce:    oldStateNonceBytes,
		Amount:        amountBytes,
		RevLockCom:    revLockComBytes,
	}

	return p.SendMessage(false, &zkpaynonce)
}

func (z *zkChannelManager) processZkPayNonce(msg *lnwire.ZkPayNonce, p lnpeer.Peer) {

	sessionID := string(msg.SessionID)
	justification := string(msg.Justification)
	stateNonce := string(msg.StateNonce)
	amount := int64(binary.LittleEndian.Uint64(msg.Amount))
	revLockCom := string(msg.RevLockCom)

	zkchLog.Debug("Just received ZkPayNonce for sessionID:", sessionID)

	paySession := PaySession{
		Amount: amount,
	}

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetMerchField(zkMerchDB, channelStateKey, &channelState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	payTokenMaskCom, merchState, err := libzkchannels.PreparePaymentMerchant(channelState, sessionID, stateNonce, revLockCom, amount, justification, merchState)
	if err != nil {
		z.failEstablishFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	paySessionDB, err := zkchanneldb.OpenZkClaimBucket(sessionID, "paysessions.db")
	if err != nil {
		zkchLog.Error(err)
	}
	err = zkchanneldb.AddField(paySessionDB, sessionID, paySession, sessionID)
	if err != nil {
		zkchLog.Error(err)
	}
	paySessionDB.Close()

	sessionIDBytes := []byte(sessionID)
	payTokenMaskComBytes := []byte(payTokenMaskCom)

	zkPayMaskCom := lnwire.ZkPayMaskCom{
		SessionID:       sessionIDBytes,
		PayTokenMaskCom: payTokenMaskComBytes,
	}
	err = p.SendMessage(false, &zkPayMaskCom)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkPayMaskCom(msg *lnwire.ZkPayMaskCom, p lnpeer.Peer, zkChannelName string) {
	zkchLog.Debug("Just received ZkPayMaskCom")

	// TODO ZKLND-53: match up sessionID to appropriate bucket
	sessionID := string(msg.SessionID)
	payTokenMaskCom := string(msg.PayTokenMaskCom)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelStateKey, &channelState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var newState libzkchannels.State
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, "newStateKey", &newState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var oldState libzkchannels.State
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, "oldStateKey", &oldState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var channelToken libzkchannels.ChannelToken
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelTokenKey, &channelToken)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var revLockCom string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, "revLockComKey", &revLockCom)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var amount int64
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, "amountKey", &amount)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	sessionIDBytes := []byte(sessionID)
	ZkPayMPC := lnwire.ZkPayMPC{
		SessionID:       sessionIDBytes,
		PayTokenMaskCom: msg.PayTokenMaskCom,
	}
	err = p.SendMessage(false, &ZkPayMPC)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	zkchLog.Debug("channelState channelTokenPkM => ", channelToken.PkM)

	pPtr := SavePointer(p)
	defer UnrefPointer(pPtr)
	isOk, custState, err := libzkchannels.PayUpdateCustomer(channelState, channelToken, oldState, newState,
		payTokenMaskCom, revLockCom, amount, custState,
		pPtr, unsafe.Pointer(C.send_cgo), unsafe.Pointer(C.receive_cgo))
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	switch isOk {
	case true:
		zkchLog.Info("MPC pay protocol succeeded")
	case false:
		zkchLog.Info("MPC pay protocol failed")
	}

	isOkBytes, err := json.Marshal(isOk)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	ZkPayMPCResult := lnwire.ZkPayMPCResult{
		SessionID: sessionIDBytes,
		IsOk:      isOkBytes,
	}
	err = p.SendMessage(false, &ZkPayMPCResult)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}
}

func (z *zkChannelManager) processZkPayMPC(msg *lnwire.ZkPayMPC, p lnpeer.Peer) {

	sessionID := string(msg.SessionID)
	payTokenMaskCom := string(msg.PayTokenMaskCom)

	zkchLog.Debug("Just received ZkPayMPC from sessionID:", sessionID)

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetMerchField(zkMerchDB, channelStateKey, &channelState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var totalReceived Total
	err = zkchanneldb.GetMerchField(zkMerchDB, totalReceivedKey, &totalReceived)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	zkchLog.Debug("channelState MerchPayOutPk => ", *channelState.MerchPayOutPk)
	zkchLog.Debug("channelState MerchDisputePk => ", *channelState.MerchDisputePk)
	zkchLog.Debug("channelState MerchStatePkM => ", *merchState.PkM)

	pPtr := SavePointer(p)
	defer UnrefPointer(pPtr)
	isOk, merchState, err := libzkchannels.PayUpdateMerchant(channelState, sessionID, payTokenMaskCom, merchState,
		pPtr, unsafe.Pointer(C.send_cgo), unsafe.Pointer(C.receive_cgo))

	// TODO: Handle this case properly
	if !isOk {
		zkchLog.Debug("MPC unsuccessful")
	}
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	zkMerchDB, err = zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchField(zkMerchDB, totalReceived, totalReceivedKey)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

}

func (z *zkChannelManager) processZkPayMPCResult(msg *lnwire.ZkPayMPCResult, p lnpeer.Peer) {

	sessionID := string(msg.SessionID)

	var isOk bool
	err := json.Unmarshal(msg.IsOk, &isOk)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	zkchLog.Debug("Just received ZkPayMPCResult. isOk: ", isOk)

	if !isOk {
		// TODO: Handle the case where MPC was unsuccessful, reinitiate UpdateMerchant?
		zkchLog.Warn("MPC was unsuccessful for sessionID: %v, terminating payment", sessionID)
		z.failZkPayFlow(p, err)
		return
	}
	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	maskedTxInputs, err := libzkchannels.PayConfirmMPCResult(sessionID, isOk, merchState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	maskedTxInputsBytes, err := json.Marshal(maskedTxInputs)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	sessionIDBytes := []byte(sessionID)

	zkPayMaskedTxInputs := lnwire.ZkPayMaskedTxInputs{
		SessionID:      sessionIDBytes,
		MaskedTxInputs: maskedTxInputsBytes,
	}

	err = p.SendMessage(false, &zkPayMaskedTxInputs)
	if err != nil {
		zkchLog.Error(err)
		return
	}
}

func (z *zkChannelManager) processZkPayMaskedTxInputs(msg *lnwire.ZkPayMaskedTxInputs, p lnpeer.Peer, zkChannelName string) {

	sessionID := string(msg.SessionID)

	var maskedTxInputs libzkchannels.MaskedTxInputs
	err := json.Unmarshal(msg.MaskedTxInputs, &maskedTxInputs)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	zkchLog.Debugf("Just received ZkPayMaskedTxInputs from sessionID: %s", sessionID)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelStateKey, &channelState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var channelToken libzkchannels.ChannelToken
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelTokenKey, &channelToken)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	isOk, custState, err := libzkchannels.PayUnmaskSigsCustomer(channelState, channelToken, maskedTxInputs, custState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	switch isOk {
	case true:
		zkchLog.Info("PayUnmaskTxCustomer successful")
	case false:
		zkchLog.Info("PayUnmaskTxCustomer failed")
	}

	zkchLog.Debug("After PayUnmaskTxCustomer, custState =>:", *custState.State)

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	// REVOKE OLD STATE
	var revState libzkchannels.RevokedState
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, "revStateKey", &revState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	sessionIDBytes := []byte(sessionID)
	revStateBytes, err := json.Marshal(revState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	zkPayRevoke := lnwire.ZkPayRevoke{
		SessionID: sessionIDBytes,
		RevState:  revStateBytes,
	}
	err = p.SendMessage(false, &zkPayRevoke)
	if err != nil {
		zkchLog.Error(err)
	}
}

func (z *zkChannelManager) processZkPayRevoke(msg *lnwire.ZkPayRevoke, p lnpeer.Peer) {
	sessionID := string(msg.SessionID)

	var revState libzkchannels.RevokedState
	err := json.Unmarshal(msg.RevState, &revState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	zkchLog.Info("Just received ZkPayRevoke from sessionID: ", sessionID)

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	var totalReceived Total
	err = zkchanneldb.GetMerchField(zkMerchDB, totalReceivedKey, &totalReceived)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	payTokenMask, payTokenMaskR, merchState, err := libzkchannels.PayValidateRevLockMerchant(sessionID, revState, merchState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	// open the paySessionDB to get the payment amount so we can add it to totalReceived
	paySessionDB, err := zkchanneldb.OpenZkClaimBucket(sessionID, "paysessions.db")
	if err != nil {
		zkchLog.Errorf("Opening bucket for pay session %v. err: %v", sessionID, err)
	}

	var paySession PaySession
	err = zkchanneldb.GetField(paySessionDB, sessionID, sessionID, &paySession)
	if err != nil {
		zkchLog.Error("GetField: ", err)
	}

	err = paySessionDB.Close()
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	totalReceived.Amount += paySession.Amount

	zkMerchDB, err = zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkchanneldb.AddMerchField(zkMerchDB, totalReceived, totalReceivedKey)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	sessionIDBytes := []byte(sessionID)
	payTokenMaskBytes := []byte(payTokenMask)
	payTokenMaskRBytes := []byte(payTokenMaskR)

	zkPayTokenMask := lnwire.ZkPayTokenMask{
		SessionID:     sessionIDBytes,
		PayTokenMask:  payTokenMaskBytes,
		PayTokenMaskR: payTokenMaskRBytes,
	}
	err = p.SendMessage(false, &zkPayTokenMask)
	if err != nil {
		zkchLog.Error(err)
	}

}

func (z *zkChannelManager) processZkPayTokenMask(msg *lnwire.ZkPayTokenMask, p lnpeer.Peer, zkChannelName string) {
	sessionID := string(msg.SessionID)
	payTokenMask := string(msg.PayTokenMask)
	payTokenMaskR := string(msg.PayTokenMaskR)

	zkchLog.Info("Just received PayTokenMask and PayTokenMaskR from sessionID: ", sessionID)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	isOk, custState, err := libzkchannels.PayUnmaskPayTokenCustomer(payTokenMask, payTokenMaskR, custState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	switch isOk {
	case true:
		zkchLog.Info("Unmask Pay Token successful")
	case false:
		zkchLog.Info("Unmask Pay Token failed")
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		z.failZkPayFlow(p, err)
		return
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
	}

	// Now that the pay session has ended, disconnect from the merchant so that
	// subsequent payments can be made on a fresh connection with a new
	// customer nodeID
	err = z.DisconnectMerchant(p.IdentityKey())
	if err != nil {
		zkchLog.Error(err)
	}
}

// CloseZkChannel broadcasts a close transaction
func (z *zkChannelManager) CloseZkChannel(notifier chainntnfs.ChainNotifier, zkChannelName string, dryRun bool) error {

	closeFromEscrow := true

	closeEscrowTx, closeEscrowTxid, err := GetSignedCustCloseTxs(zkChannelName, closeFromEscrow, z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	zkchLog.Debug("Signed closeEscrowTx =>:", closeEscrowTx)
	zkchLog.Infof("closeEscrowTx =>:", closeEscrowTxid)

	// Broadcast escrow tx on chain
	serializedTx, err := hex.DecodeString(closeEscrowTx)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	var msgTx wire.MsgTx
	err = msgTx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	if dryRun {
		zkchLog.Infof("DryRun: Not Broadcasting close transaction:",
			closeEscrowTx)
		return nil
	}

	zkchLog.Info("Broadcasting close transaction")
	err = z.PublishTransaction(&msgTx, "")
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = updateCustChannelState(z.dbPath, zkChannelName, "PendingClose")
	if err != nil {
		zkchLog.Error(err)
	}

	// Start watching for on-chain notifications of custClose
	pkScript := msgTx.TxOut[0].PkScript
	go z.waitForCustCloseConfirmations(notifier, closeEscrowTxid, pkScript, zkChannelName)

	return nil
}

func (z *zkChannelManager) waitForCustCloseConfirmations(notifier chainntnfs.ChainNotifier, custCloseTx string, pkScript []byte, zkChannelName string) {

	zkchLog.Infof("waiting for custCloseTx confirmations")
	// Wait for confirmations
	confChannel, err := z.waitForFundingWithTimeout(notifier, custCloseTx, pkScript)
	if err != nil {
		zkchLog.Infof("error waiting for custCloseTx "+
			"confirmation: %v", err)
	}

	err = updateCustChannelState(z.dbPath, zkChannelName, "ConfirmedClose")
	if err != nil {
		zkchLog.Error(err)
	}

	zkchLog.Debugf("confChannel: %#v\n", confChannel)
	zkchLog.Infof("CustCloseTx %v has 3 confirmations", custCloseTx)
}

func updateCustChannelState(DBPath string, zkChannelName string, newStatus string) error {

	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, DBPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}
	defer zkCustDB.Close()

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	switch newStatus {
	case "Open":
		custState, err = libzkchannels.CustomerChangeChannelStatusToOpen(custState)
		if err != nil {
			zkchLog.Error(err)
			return err
		}
	case "PendingClose":
		custState, err = libzkchannels.CustomerChangeChannelStatusToPendingClose(custState)
		if err != nil {
			zkchLog.Error(err)
			return err
		}
	case "ConfirmedClose":
		custState, err = libzkchannels.CustomerChangeChannelStatusToConfirmedClose(custState)
		if err != nil {
			zkchLog.Error(err)
			return err
		}
	default:
		return fmt.Errorf("unrecognised status: %v", newStatus)
	}

	err = zkchanneldb.AddCustState(zkCustDB, zkChannelName, custState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}
	return nil
}

func getCustChannelState(DBPath string, zkChannelName string) (status string, err error) {

	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, DBPath)
	if err != nil {
		zkchLog.Error(err)
		return "", err
	}
	defer zkCustDB.Close()

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		zkchLog.Error(err)
		return "", err
	}

	return custState.ChannelStatus, nil
}

func updateMerchChannelState(DBPath string, escrowTxid string, newStatus string) error {

	zkMerchDB, err := zkchanneldb.OpenMerchBucket(DBPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}
	defer zkMerchDB.Close()

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	switch newStatus {
	case "Open":
		merchState, err = libzkchannels.MerchantChangeChannelStatusToOpen(escrowTxid, merchState)
		if err != nil {
			zkchLog.Errorf("MerchantChangeChannelStatusToOpen: %v", err)
			return err
		}
	case "PendingClose":
		merchState, err = libzkchannels.MerchantChangeChannelStatusToPendingClose(escrowTxid, merchState)
		if err != nil {
			zkchLog.Errorf("MerchantChangeChannelStatusToPendingClose: %v", err)
			return err
		}
	case "ConfirmedClose":
		merchState, err = libzkchannels.MerchantChangeChannelStatusToConfirmedClose(escrowTxid, merchState)
		if err != nil {
			zkchLog.Errorf("MerchantChangeChannelStatusToConfirmedClose: %v", err)
			return err
		}
	default:
		return fmt.Errorf("unrecognised status: %v", newStatus)
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}
	return nil
}

func getMerchChannelState(DBPath string, escrowTxid string) (status string, err error) {

	zkMerchDB, err := zkchanneldb.OpenMerchBucket(DBPath)
	if err != nil {
		zkchLog.Error(err)
		return "", err
	}
	defer zkMerchDB.Close()

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		zkchLog.Error(err)
		return "", err
	}

	// Flip bytes from Little Endiand to Big Endian
	// This works because hex strings are of even size
	s := ""
	for i := 0; i < len(escrowTxid)/2; i++ {
		s = escrowTxid[i*2:i*2+2] + s
	}
	escrowTxidBigEn := s

	status, ok := (*merchState.ChannelStatusMap)[escrowTxidBigEn].(string)
	if ok != true {
		return "", fmt.Errorf("error in getMerchChannelState")
	}
	return status, nil
}

// GetSignedCustCloseTxs gets the custCloseTx and also sets closeInitiated to true
// to signal that no further payments should be made with this channel.
func GetSignedCustCloseTxs(zkChannelName string, closeEscrow bool, DBPath string) (closeEscrowTx string, closeEscrowTxid string, err error) {
	// Add a flag to zkchannelsdb to say that closeChannel has been initiated.
	// This is used to prevent another payment being made
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, DBPath)
	if err != nil {
		zkchLog.Error(err)
		return "", "", err
	}

	// Set closeInitiated to true, to prevent further payments on this channel
	err = zkchanneldb.AddField(zkCustDB, zkChannelName, true, "closeInitiatedKey")
	if err != nil {
		zkchLog.Error(err)
		return "", "", err
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	if err != nil {
		zkchLog.Error(err)
		return
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelStateKey, &channelState)
	if err != nil {
		zkchLog.Error(err)
		return "", "", err
	}

	var channelToken libzkchannels.ChannelToken
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, channelTokenKey, &channelToken)
	if err != nil {
		zkchLog.Error(err)
		return "", "", err
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error(err)
		return "", "", err
	}

	closeEscrowTx, closeEscrowTxid, custState, err = libzkchannels.ForceCustomerCloseTx(channelState, channelToken, closeEscrow, custState)
	if err != nil {
		zkchLog.Error(err)
		return "", "", err
	}

	return closeEscrowTx, closeEscrowTxid, nil

}

// MerchClose broadcasts a close transaction for a given escrow txid
func (z *zkChannelManager) MerchClose(notifier chainntnfs.ChainNotifier, escrowTxid string) error {

	// open the zkchanneldb to create signedMerchCloseTx
	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	var channelState libzkchannels.ChannelState
	err = zkchanneldb.GetMerchField(zkMerchDB, channelStateKey, &channelState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	zkchLog.Infof("escrowTxid to close =>:", escrowTxid)

	zkchLog.Debugf("\nmerchState =>:%+v", merchState)
	zkchLog.Infof("\nCloseTxMap =>:%+v", merchState.CloseTxMap)

	signedMerchCloseTx, _, merchTxid2, merchState, err := libzkchannels.ForceMerchantCloseTx(escrowTxid, merchState, channelState.ValCpfp)
	if err != nil {
		zkchLog.Errorf("ForceMerchantCloseTx:", err)
		return err
	}

	err = zkchanneldb.AddMerchState(zkMerchDB, merchState)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	err = zkMerchDB.Close()
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	zkchLog.Debug("signedMerchCloseTx =>:", signedMerchCloseTx)
	zkchLog.Debug("signedMerchCloseTxid =>:", merchTxid2)

	// Broadcast escrow tx on chain
	serializedTx, err := hex.DecodeString(signedMerchCloseTx)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	var msgTx wire.MsgTx
	err = msgTx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	fundingOut := &wire.OutPoint{
		Hash:  msgTx.TxHash(),
		Index: uint32(0),
	}
	zkchLog.Debugf("fundingOut: %v", fundingOut)

	// TODO: Rename to merchClose Info, or something more general
	ZkFundingInfo := contractcourt.ZkFundingInfo{
		FundingOut:      *fundingOut,
		PkScript:        msgTx.TxOut[0].PkScript,
		BroadcastHeight: uint32(300), // TODO ZKLND-50: Replace with actual fundingtx confirm height
	}
	zkchLog.Debugf("ZkFundingInfo: %v", ZkFundingInfo)
	zkchLog.Debugf("pkScript: %v", ZkFundingInfo.PkScript)

	zkChainWatcherCfg := contractcourt.ZkChainWatcherConfig{
		ZkFundingInfo:      ZkFundingInfo,
		IsMerch:            true,
		WatchingMerchClose: true,
		CustChannelName:    "",
		Notifier:           notifier,
	}
	zkchLog.Debugf("notifier: %v", notifier)

	if err := z.WatchNewZkChannel(zkChainWatcherCfg); err != nil {
		zkchLog.Errorf("Unable to send new ChannelPoint(%v) for "+
			"arbitration: %v", escrowTxid, err)
	}

	zkchLog.Info("Broadcasting merch close transaction")
	err = z.PublishTransaction(&msgTx, "")
	if err != nil {
		zkchLog.Infof("Couldn't publish transaction: %v", err)
		return err
	}

	// March the channel as pending close. This will prevent customer's from
	// making payments on this channel (for their own safety).
	err = updateMerchChannelState(z.dbPath, escrowTxid, "PendingClose")
	if err != nil {
		zkchLog.Infof("Couldn't updateMerchChannelState: %v", err)
		return err
	}

	// Start watching for on-chain notifications of merchClose
	pkScript := msgTx.TxOut[0].PkScript

	// Wait for on chain confirmations of merch close transaction
	z.wg.Add(1)
	go z.advanceMerchantStateAfterConfirmations(notifier, false, escrowTxid, merchTxid2, pkScript)

	return nil
}

// ZkChannelBalance returns the balance on the customer's zkchannel
func (z *zkChannelManager) ZkChannelBalance(zkChannelName string) (string, int64, int64, error) {

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkChannelBucket(zkChannelName, z.dbPath)
	if err != nil {
		zkchLog.Error("OpenZkChannelBucket: ", err)
		return "", 0, 0, err
	}

	custState, err := zkchanneldb.GetCustState(zkCustDB, zkChannelName)
	// If there are no channels set up, close the db and return 0
	if len(custState.Name) == 0 {
		err = zkCustDB.Close()
		if err != nil {
			zkchLog.Error("Close: ", err)
			return "", 0, 0, err
		}
		return "", 0, 0, nil
	}
	if err != nil {
		zkchLog.Error("GetCustState: ", err)
		return "", 0, 0, err
	}

	localBalance := custState.CustBalance
	remoteBalance := custState.MerchBalance

	var escrowTxid string
	err = zkchanneldb.GetField(zkCustDB, zkChannelName, escrowTxidKey, &escrowTxid)
	zkchLog.Infof("escrowTxid: ", escrowTxid)

	if err != nil {
		zkchLog.Error("GetField: ", err)
		return "", 0, 0, err
	}

	err = zkCustDB.Close()
	if err != nil {
		zkchLog.Error("Close: ", err)
		return "", 0, 0, err
	}

	return escrowTxid, localBalance, remoteBalance, err
}

// TotalReceived returns the balance on the merchant's zkchannel
func (z *zkChannelManager) TotalReceived() (int64, error) {

	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return 0, err
	}

	var totalReceived Total
	err = zkchanneldb.GetMerchField(zkMerchDB, totalReceivedKey, &totalReceived)
	if err != nil {
		zkchLog.Error(err)
		return 0, err
	}

	err = zkMerchDB.Close()

	return totalReceived.Amount, err
}

// ZkInfo returns info about this zklnd node
func (z *zkChannelManager) ZkInfo() (string, error) {

	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return "", err
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		zkchLog.Error(err)
		return "", err
	}

	err = zkMerchDB.Close()

	return *merchState.PkM, err
}

type ListOfZkChannels struct {
	ChannelID    []string
	ChannelToken []libzkchannels.ChannelToken
}

// ListZkChannels returns a list of the merchant's zkchannels
func (z *zkChannelManager) ListZkChannels() (ListOfZkChannels, error) {

	zkMerchDB, err := zkchanneldb.OpenMerchBucket(z.dbPath)
	if err != nil {
		zkchLog.Error(err)
		return ListOfZkChannels{}, err
	}

	merchState, err := zkchanneldb.GetMerchState(zkMerchDB)
	if err != nil {
		zkchLog.Error(err)
		return ListOfZkChannels{}, err
	}

	if merchState.CloseTxMap == nil {
		return ListOfZkChannels{}, errors.New("Something went wrong retrieving Merchant State")
	}

	var zkChannels map[string]libzkchannels.ChannelToken
	err = zkchanneldb.GetMerchField(zkMerchDB, zkChannelsKey, &zkChannels)
	if err != nil {
		zkchLog.Error("zkChannels", err)
		return ListOfZkChannels{}, err
	}

	var channelIDs []string
	var channelTokens []libzkchannels.ChannelToken
	for channelID, channelToken := range zkChannels {
		channelIDs = append(channelIDs, channelID)
		channelTokens = append(channelTokens, channelToken)
	}

	ListOfZkChannels := ListOfZkChannels{
		ChannelID:    channelIDs,
		ChannelToken: channelTokens,
	}

	err = zkMerchDB.Close()

	return ListOfZkChannels, err
}

// DetermineIfCust is used to check the user is a customer
func DetermineIfCust() (bool, error) {
	if user, err := CustOrMerch(); user == "cust" {
		if err != nil {
			zkchLog.Error(err)
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// DetermineIfMerch is used to check the user is a merchant
func DetermineIfMerch() (bool, error) {
	if user, err := CustOrMerch(); user == "merch" {
		if err != nil {
			zkchLog.Error(err)
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// CustOrMerch determines if the user is a customer or merchant,
// based on whether they have zkcust.db or zkmerch.db set up
func CustOrMerch() (string, error) {

	var custdbExists, merchdbExists bool
	if _, err := os.Stat("zkcust.db"); err == nil {
		custdbExists = true
	}
	if _, err := os.Stat("zkmerch.db"); err == nil {
		merchdbExists = true
	}

	if custdbExists && merchdbExists {
		return "both", fmt.Errorf("Cannot run both a Customer and Merchant node. " +
			"Both zkcust.cb and zkmerch.db exist.")
	} else if custdbExists {
		return "cust", nil
	} else if merchdbExists {
		return "merch", nil
	}
	return "neither", fmt.Errorf("neither zkcust.db or zkmerch.db found")
}

// StringInSlice checks if a string exists in a slice of strings
func StringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

// ChannelExists checks if a channel with that name has been established
func ChannelExists(zkChannelName string) bool {
	zkChannelList, err := zkchanneldb.Buckets("zkcust.db")
	if err != nil {
		zkchLog.Error("error opening zkcust.db ", err)
		return false
	}
	return StringInSlice(zkChannelName, zkChannelList)
}

// CustClaim sweeps a customer's output from a close tx.
func (z *zkChannelManager) CustClaim(wallet *lnwallet.LightningWallet, notifier chainntnfs.ChainNotifier, escrowTxid string) error {

	zkchLog.Debugf("zkChannelManager CustClaim inputs: ", escrowTxid)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.OpenZkClaimBucket(escrowTxid, "zkclaim.db")
	if err != nil {
		zkchLog.Error("OpenZkChannelBucket: ", err)
		return err
	}

	var signedCustClaimTx string
	err = zkchanneldb.GetField(zkCustDB, escrowTxid, "signedCustClaimTxKey", &signedCustClaimTx)
	if err != nil {
		zkchLog.Error("GetField: ", err)
		return err
	}

	err = zkCustDB.Close()

	zkchLog.Debugf("signedCustClaimTx: %#v", signedCustClaimTx)

	// Broadcast escrow tx on chain
	serializedTx, err := hex.DecodeString(signedCustClaimTx)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	var msgTx wire.MsgTx
	err = msgTx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	zkchLog.Info("Broadcasting merch close transaction")
	err = z.PublishTransaction(&msgTx, "")
	if err != nil {
		zkchLog.Infof("Couldn't publish transaction: %v", err)
		return err
	}

	return nil
}

// MerchClaim sweeps a merchant's output from a close tx.
func (z *zkChannelManager) MerchClaim(wallet *lnwallet.LightningWallet, notifier chainntnfs.ChainNotifier, escrowTxid string) error {

	zkchLog.Debugf("zkChannelManager MerchClaim inputs: ", escrowTxid)

	// open the zkchanneldb to load custState
	zkMerchClaimDB, err := zkchanneldb.OpenZkClaimBucket(escrowTxid, "zkclaim.db")
	if err != nil {
		zkchLog.Error("OpenZkChannelBucket: ", err)
		return err
	}

	signedMerchClaimTx, err := zkchanneldb.GetStringField(zkMerchClaimDB, escrowTxid, "signedMerchClaimTxKey")
	if err != nil {
		zkchLog.Error("GetField: ", err)
		return err
	}

	err = zkMerchClaimDB.Close()

	zkchLog.Debugf("signedMerchClaimTx: %#v", signedMerchClaimTx)

	// Broadcast escrow tx on chain
	serializedTx, err := hex.DecodeString(signedMerchClaimTx)
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	var msgTx wire.MsgTx
	err = msgTx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		zkchLog.Error(err)
		return err
	}

	zkchLog.Info("Broadcasting merch close transaction")
	err = z.PublishTransaction(&msgTx, "")
	if err != nil {
		zkchLog.Infof("Couldn't publish transaction: %v", err)
		return err
	}

	return nil
}
