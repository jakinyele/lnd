package lnd

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/libzkchannels"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zkchanneldb"
)

type zkChannelManager struct {
}

func (z *zkChannelManager) initZkEstablish(merchPubKey string, custBal int64, merchBal int64, p lnpeer.Peer) {

	inputSats := int64(10000)

	channelToken, custState, err := libzkchannels.InitCustomer(fmt.Sprintf("\"%v\"", merchPubKey), custBal, merchBal, "cust")
	_ = err
	// assert.Nil(t, err)

	cust_utxo_txid := "f4df16149735c2963832ccaa9627f4008a06291e8b932c2fc76b3a5d62d462e1"

	custSk := fmt.Sprintf("%v", custState.SkC)
	custPk := fmt.Sprintf("%v", custState.PkC)
	revLock := fmt.Sprintf("%v", custState.RevLock)

	merchPk := fmt.Sprintf("%v", merchPubKey)
	// changeSk := "4157697b6428532758a9d0f9a73ce58befe3fd665797427d1c5bb3d33f6a132e"
	changePk := "037bed6ab680a171ef2ab564af25eff15c0659313df0bbfb96414da7c7d1e65882"

	zkchLog.Info("custSk :=> ", custSk)
	fmt.Println("custPk :=> ", custPk)
	fmt.Println("merchPk :=> ", merchPk)

	signedEscrowTx, escrowTxid, escrowPrevout, err := libzkchannels.FormEscrowTx(cust_utxo_txid, 0, inputSats, custBal, custSk, custPk, merchPk, changePk)
	// assert.Nil(t, err)

	fmt.Println("escrow txid => ", escrowTxid)
	fmt.Println("escrow prevout => ", escrowPrevout)
	fmt.Println("signedEscrowTx => ", signedEscrowTx)

	_, _ = channelToken, custState
	zkchLog.Infof("Generated channelToken and custState")

	// TODO: Write a function to handle the storing of variables in zkchanneldb
	// Add variables to zkchannelsdb
	zkCustDB, err := zkchanneldb.SetupZkCustDB()

	custStateBytes, _ := json.Marshal(custState)
	zkchanneldb.AddCustState(zkCustDB, custStateBytes)

	channelTokenBytes, _ := json.Marshal(channelToken)
	zkchanneldb.AddCustField(zkCustDB, channelTokenBytes, "channelTokenKey")

	// custSkBytes, _ := json.Marshal(custSk)
	// zkchanneldb.AddCustField(zkCustDB, custSkBytes, "custSkKey")

	merchPkBytes, _ := json.Marshal(merchPk)
	zkchanneldb.AddCustField(zkCustDB, merchPkBytes, "merchPkKey")

	custBalBytes, _ := json.Marshal(custBal)
	zkchanneldb.AddCustField(zkCustDB, custBalBytes, "custBalKey")

	merchBalBytes, _ := json.Marshal(merchBal)
	zkchanneldb.AddCustField(zkCustDB, merchBalBytes, "merchBalKey")

	escrowTxidBytes, _ := json.Marshal(escrowTxid)
	zkchanneldb.AddCustField(zkCustDB, escrowTxidBytes, "escrowTxidKey")

	escrowPrevoutBytes, _ := json.Marshal(escrowPrevout)
	zkchanneldb.AddCustField(zkCustDB, escrowPrevoutBytes, "escrowPrevoutKey")

	zkCustDB.Close()

	zkchLog.Infof("Saved custState and channelToken")

	// Convert fields into bytes
	escrowTxidBytes = []byte(escrowTxid)
	custPkBytes := []byte(custPk)
	escrowPrevoutBytes = []byte(escrowPrevout)
	revLockBytes := []byte(revLock)

	custBalBytes = make([]byte, 8)
	binary.LittleEndian.PutUint64(custBalBytes, uint64(custBal))

	merchBalBytes = make([]byte, 8)
	binary.LittleEndian.PutUint64(merchBalBytes, uint64(merchBal))

	zkEstablishOpen := lnwire.ZkEstablishOpen{
		EscrowTxid:    escrowTxidBytes,
		CustPk:        custPkBytes,
		EscrowPrevout: escrowPrevoutBytes,
		RevLock:       revLockBytes,
		CustBal:       custBalBytes,
		MerchBal:      merchBalBytes,
	}

	p.SendMessage(false, &zkEstablishOpen)

}

func (z *zkChannelManager) processZkEstablishOpen(msg *lnwire.ZkEstablishOpen, p lnpeer.Peer) {

	// NOTE: For now, toSelfDelay is hardcoded
	toSelfDelay := "cf05"

	zkchLog.Info("Just received ZkEstablishOpen with length: ", len(msg.EscrowTxid))

	// Convert variables received
	escrowTxid := string(msg.EscrowTxid)
	custPk := string(msg.CustPk)
	escrowPrevout := string(msg.EscrowPrevout)
	revLock := string(msg.RevLock)

	zkchLog.Info("msg.CustBal: ", msg.CustBal)
	zkchLog.Info("msg.MerchBal: ", msg.MerchBal)

	custBal := int64(binary.LittleEndian.Uint64(msg.CustBal))
	merchBal := int64(binary.LittleEndian.Uint64(msg.MerchBal))

	zkchLog.Info("custBal =>:", custBal)
	zkchLog.Info("merchBal =>:", merchBal)

	fmt.Println("received escrow txid => ", escrowTxid)

	// TODO: If the variables are not checked, they can be saved directly, skipping the step above/

	// Add variables to zkchannelsdb
	zkMerchDB, err := zkchanneldb.SetupZkMerchDB()

	toSelfDelayBytes, _ := json.Marshal(toSelfDelay)
	zkchanneldb.AddMerchField(zkMerchDB, toSelfDelayBytes, "toSelfDelayKey")

	custPkBytes, _ := json.Marshal(custPk)
	zkchanneldb.AddMerchField(zkMerchDB, custPkBytes, "custPkKey")

	custBalBytes, _ := json.Marshal(custBal)
	zkchanneldb.AddMerchField(zkMerchDB, custBalBytes, "custBalKey")

	merchBalBytes, _ := json.Marshal(merchBal)
	zkchanneldb.AddMerchField(zkMerchDB, merchBalBytes, "merchBalKey")

	escrowTxidBytes, _ := json.Marshal(escrowTxid)
	zkchanneldb.AddMerchField(zkMerchDB, escrowTxidBytes, "escrowTxidKey")

	escrowPrevoutBytes, _ := json.Marshal(escrowPrevout)
	zkchanneldb.AddMerchField(zkMerchDB, escrowPrevoutBytes, "escrowPrevoutKey")

	revLockBytes, _ := json.Marshal(revLock)
	zkchanneldb.AddMerchField(zkMerchDB, revLockBytes, "revLockKey")

	zkMerchDB.Close()

	// open the zkchanneldb to load merchState and channelState
	zkMerchDB, err = zkchanneldb.SetupZkMerchDB()

	// read merchState from ZkMerchDB
	var merchStateBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("merchStateKey"))
		merchStateBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchState libzkchannels.MerchState
	err = json.Unmarshal(merchStateBytes, &merchState)

	// read channelState from ZkMerchDB
	var channelStateBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("channelStateKey"))
		channelStateBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	// TODO: Since we already have channelStateBytes above, we might not need to convert it back and forth again
	var channelState libzkchannels.ChannelState
	err = json.Unmarshal(channelStateBytes, &channelState)

	zkMerchDB.Close()

	merchClosePk := fmt.Sprintf("%v", *merchState.PayoutPk)

	// Convert fields into bytes
	merchClosePkBytes := []byte(merchClosePk)
	toSelfDelayBytes = []byte(toSelfDelay)
	channelStateBytes, _ = json.Marshal(channelState)

	zkchLog.Info("converting done")

	zkEstablishAccept := lnwire.ZkEstablishAccept{
		ToSelfDelay:   toSelfDelayBytes,
		MerchPayoutPk: merchClosePkBytes,
		ChannelState:  channelStateBytes,
	}
	p.SendMessage(false, &zkEstablishAccept)

}

func (z *zkChannelManager) processZkEstablishAccept(msg *lnwire.ZkEstablishAccept, p lnpeer.Peer) {

	zkchLog.Info("Just received ZkEstablishAccept.ToSelfDelay with length: ", len(msg.ToSelfDelay))

	toSelfDelay := string(msg.ToSelfDelay)
	merchClosePk := string(msg.MerchPayoutPk)

	var channelState libzkchannels.ChannelState
	err := json.Unmarshal(msg.ChannelState, &channelState)

	// TODO: Might not have to convert back and forth between bytes here
	// Add variables to zkchannelsdb
	zkCustDB, err := zkchanneldb.SetupZkCustDB()

	channelStateBytes, _ := json.Marshal(channelState)
	zkchanneldb.AddCustField(zkCustDB, channelStateBytes, "channelStateKey")

	zkCustDB.Close()

	// open the zkchanneldb to load custState
	zkCustDB, err = zkchanneldb.SetupZkCustDB()

	// read custState from ZkCustDB
	var custStateBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("custStateKey"))
		custStateBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var custState libzkchannels.CustState
	err = json.Unmarshal(custStateBytes, &custState)
	zkchLog.Info("processEstablish, loaded custState =>:", custState)

	// read merchPkBytes from ZkCustDB
	var merchPkBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("merchPkKey"))
		merchPkBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchPk string
	err = json.Unmarshal(merchPkBytes, &merchPk)
	zkchLog.Info("processEstablish, loaded merchPk =>:", merchPk)

	// read escrowTxidBytes from ZkCustDB
	var escrowTxidBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("escrowTxidKey"))
		escrowTxidBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var escrowTxid string
	err = json.Unmarshal(escrowTxidBytes, &escrowTxid)
	zkchLog.Info("processEstablish, loaded escrowTxid =>:", escrowTxid)

	// read custBalBytes from ZkCustDB
	var custBalBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("custBalKey"))
		custBalBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var custBal int64
	err = json.Unmarshal(custBalBytes, &custBal)
	zkchLog.Info("processEstablish, loaded custBal =>:", custBal)

	// read merchBalBytes from ZkCustDB
	var merchBalBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("merchBalKey"))
		merchBalBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchBal int64
	err = json.Unmarshal(merchBalBytes, &merchBal)
	zkchLog.Info("processEstablish, loaded merchBal =>:", merchBal)

	zkCustDB.Close()

	custSk := fmt.Sprintf("%v", custState.SkC)
	custPk := fmt.Sprintf("%v", custState.PkC)
	custClosePk := fmt.Sprintf("%v", custState.PayoutPk)

	merchTxPreimage, err := libzkchannels.FormMerchCloseTx(escrowTxid, custPk, merchPk, merchClosePk, custBal, merchBal, toSelfDelay)

	zkchLog.Info("merch TxPreimage => ", merchTxPreimage)

	custSig, err := libzkchannels.CustomerSignMerchCloseTx(custSk, merchTxPreimage)

	zkchLog.Info("custSig on merchCloseTx=> ", custSig)

	// Convert variables to bytes before sending
	// TODO: Delete merchTxPreimageBytes if never used by merch
	// merchTxPreimageBytes := []byte(merchTxPreimage)
	custSigBytes := []byte(custSig)
	custClosePkBytes := []byte(custClosePk)

	zkEstablishMCloseSigned := lnwire.ZkEstablishMCloseSigned{
		// MerchTxPreimage: merchTxPreimageBytes,
		CustSig:     custSigBytes,
		CustClosePk: custClosePkBytes,
	}
	p.SendMessage(false, &zkEstablishMCloseSigned)

}

func (z *zkChannelManager) processZkEstablishMCloseSigned(msg *lnwire.ZkEstablishMCloseSigned, p lnpeer.Peer) {

	zkchLog.Info("Just received MCloseSigned.CustSig with length: ", len(msg.CustSig))

	// Convert variables received
	custSig := string(msg.CustSig)
	custClosePk := string(msg.CustClosePk)

	// open the zkchanneldb to load merchState
	zkMerchDB, err := zkchanneldb.SetupZkMerchDB()

	// read merchState from ZkMerchDB
	var merchStateBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("merchStateKey"))
		merchStateBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchState libzkchannels.MerchState
	err = json.Unmarshal(merchStateBytes, &merchState)

	// read escrowTxidBytes from ZkMerchDB
	var toSelfDelayBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("toSelfDelayKey"))
		toSelfDelayBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var toSelfDelay string
	err = json.Unmarshal(toSelfDelayBytes, &toSelfDelay)
	zkchLog.Info("processEstablishMCloseSigned, loaded toSelfDelay =>:", toSelfDelay)

	// read escrowTxidBytes from ZkMerchDB
	var escrowTxidBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("escrowTxidKey"))
		escrowTxidBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var escrowTxid string
	err = json.Unmarshal(escrowTxidBytes, &escrowTxid)
	zkchLog.Info("processEstablishMCloseSigned, loaded escrowTxid =>:", escrowTxid)

	// read custPkBytes from ZkMerchDB
	var custPkBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("custPkKey"))
		custPkBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var custPk string
	err = json.Unmarshal(custPkBytes, &custPk)
	zkchLog.Info("processEstablishMCloseSigned, loaded custPk =>:", custPk)

	// read custBalBytes from ZkMerchDB
	var custBalBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("custBalKey"))
		custBalBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var custBal int64
	err = json.Unmarshal(custBalBytes, &custBal)
	zkchLog.Info("processEstablishMCloseSigned, loaded custBal =>:", custBal)

	// read merchBalBytes from ZkMerchDB
	var merchBalBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("merchBalKey"))
		merchBalBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchBal int64
	err = json.Unmarshal(merchBalBytes, &merchBal)
	zkchLog.Info("processEstablishMCloseSigned, loaded merchBal =>:", merchBal)

	// read escrowPrevoutBytes from ZkMerchDB
	var escrowPrevoutBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("escrowPrevoutKey"))
		escrowPrevoutBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var escrowPrevout string
	err = json.Unmarshal(escrowPrevoutBytes, &escrowPrevout)
	zkchLog.Info("processEstablishMCloseSigned, loaded escrowPrevout =>:", escrowPrevout)

	// read escrowPrevoutBytes from ZkMerchDB
	var revLockBytes []byte
	err = zkMerchDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.MerchBucket).Cursor()
		_, v := c.Seek([]byte("revLockKey"))
		revLockBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var revLock string
	err = json.Unmarshal(revLockBytes, &revLock)
	zkchLog.Info("processEstablishMCloseSigned, loaded revLock =>:", revLock)

	zkMerchDB.Close()

	merchPk := fmt.Sprintf("%v", *merchState.PkM)
	merchSk := fmt.Sprintf("%v", *merchState.SkM)
	merchClosePk := fmt.Sprintf("%v", *merchState.PayoutPk)

	fmt.Println("Variables going into MerchantSignMerchClose:", escrowTxid, custPk, merchPk, merchClosePk, custBal, merchBal, toSelfDelay, custSig, merchSk)
	signedMerchCloseTx, merchTxid, merchPrevout, err := libzkchannels.MerchantSignMerchCloseTx(escrowTxid, custPk, merchPk, merchClosePk, custBal, merchBal, toSelfDelay, custSig, merchSk)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Merchant has signed merch close tx => ", signedMerchCloseTx)
	fmt.Println("merch txid = ", merchTxid)
	fmt.Println("merch prevout = ", merchPrevout)

	// MERCH SIGN CUST CLOSE

	txInfo := libzkchannels.FundingTxInfo{
		EscrowTxId:    escrowTxid,
		EscrowPrevout: escrowPrevout,
		MerchTxId:     merchTxid,
		MerchPrevout:  merchPrevout,
		InitCustBal:   custBal,
		InitMerchBal:  merchBal,
	}

	fmt.Println("RevLock => ", revLock)

	escrowSig, merchSig, err := libzkchannels.MerchantSignInitCustCloseTx(txInfo, revLock, custPk, custClosePk, toSelfDelay, merchState)
	// assert.Nil(t, err)
	fmt.Println("escrow sig: ", escrowSig)
	fmt.Println("merch sig: ", merchSig)

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

	p.SendMessage(false, &zkEstablishCCloseSigned)

}

func (z *zkChannelManager) processZkEstablishCCloseSigned(msg *lnwire.ZkEstablishCCloseSigned, p lnpeer.Peer) {

	zkchLog.Info("Just received CCloseSigned with length: ", len(msg.EscrowSig))

	// Convert variables received
	escrowSig := string(msg.EscrowSig)
	merchSig := string(msg.MerchSig)
	merchTxid := string(msg.MerchTxid)
	merchPrevout := string(msg.MerchPrevout)

	fmt.Println("escrow sig: ", escrowSig)
	fmt.Println("merch sig: ", merchSig)

	// open the zkchanneldb to load custState
	zkCustDB, err := zkchanneldb.SetupZkCustDB()

	// read custState from ZkCustDB
	var custStateBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("custStateKey"))
		custStateBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var custState libzkchannels.CustState
	err = json.Unmarshal(custStateBytes, &custState)

	// read merchPkBytes from ZkCustDB
	var merchPkBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("merchPkKey"))
		merchPkBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchPk string
	err = json.Unmarshal(merchPkBytes, &merchPk)

	// read escrowTxidBytes from ZkCustDB
	var escrowTxidBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("escrowTxidKey"))
		escrowTxidBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var escrowTxid string
	err = json.Unmarshal(escrowTxidBytes, &escrowTxid)

	// read escrowPrevoutBytes from ZkCustDB
	var escrowPrevoutBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("escrowPrevoutKey"))
		escrowPrevoutBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var escrowPrevout string
	err = json.Unmarshal(escrowPrevoutBytes, &escrowPrevout)

	// read channelStateBytes from ZkCustDB
	var channelStateBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("channelStateKey"))
		channelStateBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var channelState libzkchannels.ChannelState
	err = json.Unmarshal(channelStateBytes, &channelState)
	zkchLog.Info("processEstablishCCloseSigned, loaded channelState =>:", channelState)

	// read channelTokenBytes from ZkCustDB
	var channelTokenBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("channelTokenKey"))
		channelTokenBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var channelToken libzkchannels.ChannelToken
	err = json.Unmarshal(channelTokenBytes, &channelToken)

	// read custBalBytes from ZkCustDB
	var custBalBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("custBalKey"))
		custBalBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var custBal int64
	err = json.Unmarshal(custBalBytes, &custBal)
	zkchLog.Info("processEstablishCCloseSigned, loaded custBal =>:", custBal)

	// read merchBalBytes from ZkCustDB
	var merchBalBytes []byte
	err = zkCustDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(zkchanneldb.CustBucket).Cursor()
		_, v := c.Seek([]byte("merchBalKey"))
		merchBalBytes = v
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var merchBal int64
	err = json.Unmarshal(merchBalBytes, &merchBal)
	zkchLog.Info("processEstablishCCloseSigned, loaded merchBal =>:", merchBal)

	zkCustDB.Close()

	txInfo := libzkchannels.FundingTxInfo{
		EscrowTxId:    escrowTxid,
		EscrowPrevout: escrowPrevout,
		MerchTxId:     merchTxid,
		MerchPrevout:  merchPrevout,
		InitCustBal:   custBal,
		InitMerchBal:  merchBal,
	}

	fmt.Println("Variables going into CustomerSignInitCustCloseTx:", txInfo, channelState, channelToken, escrowSig, merchSig, custState)

	isOk, channelToken, custState, err := libzkchannels.CustomerSignInitCustCloseTx(txInfo, channelState, channelToken, escrowSig, merchSig, custState)
	if err != nil {
		log.Fatal(err)
	}
	zkchLog.Info("Are merch sigs okay? => ", isOk)

	// If merchSigs are okay, broadcast escrowTx
	// When escrowTx is broadcast on chain, then send "Funding Locked" msg

	// TEMPORARY DUMMY MESSAGE
	paymentBytes := []byte{'d', 'u', 'm', 'm', 'y'}
	zkEstablishFundingLocked := lnwire.ZkEstablishFundingLocked{
		Payment: paymentBytes,
	}
	p.SendMessage(false, &zkEstablishFundingLocked)

}

func (z *zkChannelManager) processZkEstablishFundingLocked(msg *lnwire.ZkEstablishFundingLocked, p lnpeer.Peer) {

	zkchLog.Info("Just received FundingLocked with length: ", len(msg.Payment))

	// // To load from rpc message
	var payment string
	err := json.Unmarshal(msg.Payment, &payment)
	_ = err

	// TEMPORARY DUMMY MESSAGE
	paymentBytes := []byte{'d', 'u', 'm', 'm', 'y'}
	zkEstablishFundingConfirmed := lnwire.ZkEstablishFundingConfirmed{
		Payment: paymentBytes,
	}
	p.SendMessage(false, &zkEstablishFundingConfirmed)

}

func (z *zkChannelManager) processZkEstablishFundingConfirmed(msg *lnwire.ZkEstablishFundingConfirmed, p lnpeer.Peer) {

	zkchLog.Info("Just received FundingConfirmed with length: ", len(msg.Payment))

	// // To load from rpc message
	var payment string
	err := json.Unmarshal(msg.Payment, &payment)
	_ = err

	// TEMPORARY DUMMY MESSAGE
	paymentBytes := []byte{'d', 'u', 'm', 'm', 'y'}
	zkEstablishCustActivated := lnwire.ZkEstablishCustActivated{
		Payment: paymentBytes,
	}
	p.SendMessage(false, &zkEstablishCustActivated)

}

func (z *zkChannelManager) processZkEstablishCustActivated(msg *lnwire.ZkEstablishCustActivated, p lnpeer.Peer) {

	zkchLog.Info("Just received CustActivated with length: ", len(msg.Payment))

	// // To load from rpc message
	var payment string
	err := json.Unmarshal(msg.Payment, &payment)
	_ = err

	// TEMPORARY DUMMY MESSAGE
	paymentBytes := []byte{'d', 'u', 'm', 'm', 'y'}
	zkEstablishPayToken := lnwire.ZkEstablishPayToken{
		Payment: paymentBytes,
	}
	p.SendMessage(false, &zkEstablishPayToken)

}

func (z *zkChannelManager) processZkEstablishPayToken(msg *lnwire.ZkEstablishPayToken, p lnpeer.Peer) {

	zkchLog.Info("Just received PayToken with length: ", len(msg.Payment))

	// // To load from rpc message
	var payment string
	err := json.Unmarshal(msg.Payment, &payment)
	_ = err

	// // TEMPORARY DUMMY MESSAGE
	// paymentBytes := []byte{'d', 'u', 'm', 'm', 'y'}
	// zkEstablish := lnwire.ZkEstablish{
	// 	Payment: paymentBytes,
	// }
	// p.SendMessage(false, &zkEstablish)

}

func (z *zkChannelManager) processZkPayProof(msg *lnwire.ZkPayProof, p lnpeer.Peer) {

	zkchLog.Info("Just received ZkPayProof with length: ", len(msg.Payment))

	// // To load from rpc message
	var payment string
	err := json.Unmarshal(msg.Payment, &payment)
	_ = err

	// TEMPORARY DUMMY MESSAGE
	closeTokenBytes := []byte{'d', 'u', 'm', 'm', 'y', 'y', 'y', 'y'}

	zkPayClose := lnwire.ZkPayClose{
		CloseToken: closeTokenBytes,
	}
	p.SendMessage(false, &zkPayClose)

}

func (z *zkChannelManager) processZkPayClose(msg *lnwire.ZkPayClose, p lnpeer.Peer) {

	zkchLog.Info("Just received ZkPayClose with length: ", len(msg.CloseToken))

	// TEMPORARY dummy message
	revokeTokenBytes := []byte{'d', 'u', 'm', 'm', 'y'}

	zkPayRevoke := lnwire.ZkPayRevoke{
		RevokeToken: revokeTokenBytes,
	}
	p.SendMessage(false, &zkPayRevoke)

}

func (z *zkChannelManager) processZkPayRevoke(msg *lnwire.ZkPayRevoke, p lnpeer.Peer) {
	zkchLog.Info("Just received ZkPayRevoke with length: ", len(msg.RevokeToken))

	// TEMPORARY dummy message
	payTokenBytes := []byte{'d', 'u', 'm', 'm', 'y'}

	zkPayToken := lnwire.ZkPayToken{
		PayToken: payTokenBytes,
	}
	p.SendMessage(false, &zkPayToken)
}

func (z *zkChannelManager) processZkPayToken(msg *lnwire.ZkPayToken, p lnpeer.Peer) {
	zkchLog.Info("Just received ZkPayToken with length: ", len(msg.PayToken))
}
