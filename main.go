package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	shell "github.com/ipfs/go-ipfs-api"
	"github.com/joho/godotenv"
)

// Block represents each 'item' in the blockchain
type Block struct {
	Index        int
	Timestamp    string
	Document     Document
	Hash         string
	PrevHash     string
	IPFSHash     string
	PrevIPFSHash string
}

type Document struct {
	Title    string
	IPFSHash string
}

var (
	// Blockchain is a series of validated Blocks
	Blockchain []Block
	sh         *shell.Shell
	mutex      = &sync.Mutex{}
	ipfsTopic  = "aleksen/pubsubchain"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(run())

}

// web server
func run() error {
	sh = shell.NewShell("localhost:5001")
	// Validate that connection is active
	if _, err := sh.ID(); err != nil {
		fmt.Println("You are probably not running the IPFS daemon")
		return err
	}

	sub, err := sh.PubSubSubscribe(ipfsTopic)
	if err != nil {
		return err
	}
	go func() {
		for {
			if err := listenToTopic(sub); err != nil {
				log.Println(err)
			}
		}
	}()

	mux := makeMuxRouter()
	httpPort := os.Getenv("PORT")
	log.Println("HTTP Server Listening on port :", httpPort)
	s := &http.Server{
		Addr:           ":" + httpPort,
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	if err := s.ListenAndServe(); err != nil {
		return err
	}

	return nil
}

// create handlers
func makeMuxRouter() http.Handler {
	muxRouter := mux.NewRouter()
	muxRouter.HandleFunc("/", handleGetBlockchain).Methods("GET")
	muxRouter.HandleFunc("/", handleWriteBlock).Methods("POST")
	return muxRouter
}

// write blockchain when we receive an http request
func handleGetBlockchain(w http.ResponseWriter, r *http.Request) {
	bytes, err := json.MarshalIndent(Blockchain, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, string(bytes))
}

// takes JSON payload as an input for heart rate (BPM)
func handleWriteBlock(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var msg Document

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&msg); err != nil {
		respondWithJSON(w, r, http.StatusBadRequest, r.Body)
		return
	}
	defer r.Body.Close()

	mutex.Lock()
	var prevBlock *Block
	if len(Blockchain) > 0 {
		prevBlock = &Blockchain[len(Blockchain)-1]

	}
	newBlock, err := generateBlock(prevBlock, msg)
	if err != nil {
		respondWithJSON(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	if len(Blockchain) == 0 || isBlockValid(newBlock, *prevBlock) {
		Blockchain = append(Blockchain, newBlock)
	}
	mutex.Unlock()

	respondWithJSON(w, r, http.StatusCreated, newBlock)
	go publishBlockchain()
}

func respondWithJSON(w http.ResponseWriter, r *http.Request, code int, payload interface{}) {
	response, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("HTTP 500: Internal Server Error"))
		return
	}
	w.WriteHeader(code)
	w.Write(response)
}

// make sure block is valid by checking index, and comparing the hash of the previous block
func isBlockValid(newBlock, oldBlock Block) bool {
	if oldBlock.Index+1 != newBlock.Index {
		return false
	}

	if oldBlock.Hash != newBlock.PrevHash {
		return false
	}

	if calculateHash(newBlock) != newBlock.Hash {
		return false
	}

	return true
}

// SHA256 hasing
func calculateHash(block Block) string {
	record := strconv.Itoa(block.Index) + block.Timestamp + block.Document.Title + block.Document.IPFSHash + block.PrevHash
	h := sha256.New()
	h.Write([]byte(record))
	hashed := h.Sum(nil)
	return hex.EncodeToString(hashed)
}

// create a new block using previous block's hash
func generateBlock(oldBlock *Block, document Document) (Block, error) {

	var newBlock Block

	t := time.Now()

	if oldBlock != nil {
		newBlock.Index = oldBlock.Index + 1
		newBlock.PrevHash = oldBlock.Hash
		newBlock.PrevIPFSHash = oldBlock.IPFSHash
	}
	newBlock.Timestamp = t.String()
	newBlock.Document = document
	newBlock.Hash = calculateHash(newBlock)
	data, err := json.Marshal(newBlock)
	if err != nil {
		return newBlock, err
	}
	r := bytes.NewReader(data)
	ipfsHash, err := sh.Add(r)
	newBlock.IPFSHash = ipfsHash
	return newBlock, err
}

func publishBlockchain() {
	if len(Blockchain) == 0 {
		return
	}
	data, err := json.Marshal(Blockchain[len(Blockchain)-1])
	if err != nil {
		log.Println(err)
		return
	}
	sh.PubSubPublish(ipfsTopic, string(data))
}

func downloadBlock(ipfsHash string) (b Block, err error) {
	reader, err := sh.Cat(ipfsHash)
	if err != nil {
		return
	}
	data, err := ioutil.ReadAll(reader)
	reader.Close()
	if err != nil {
		return
	}
	err = json.Unmarshal(data, &b)
	return
}

func downloadBlocks(incomingData []byte) ([]Block, error) {
	mutex.Lock()
	defer mutex.Unlock()
	highestLocalIndex := -1
	if len(Blockchain) > 0 {
		highestLocalIndex = Blockchain[len(Blockchain)-1].Index
	}
	var incomingBlock Block
	err := json.Unmarshal(incomingData, &incomingBlock)
	if err != nil {
		return nil, err
	}
	if incomingBlock.Index <= highestLocalIndex {
		return nil, errors.New("Too old block")
	}

	bs := []Block{incomingBlock}
	for index := incomingBlock.Index - 1; index > highestLocalIndex; index-- {
		previousBlock, err := downloadBlock(bs[0].PrevIPFSHash)
		if err != nil {
			return nil, err
		}
		bs = append([]Block{previousBlock}, bs...)
	}
	return bs, nil
}

func listenToTopic(sub *shell.PubSubSubscription) error {
	next, err := sub.Next()
	if err != nil {
		return err
	}
	incomingBlocks, err := downloadBlocks(next.Data)
	if err != nil {
		return err
	}
	mutex.Lock()
	defer mutex.Unlock()
	newBlockchain := append(Blockchain, incomingBlocks...)
	if isChainValid(newBlockchain) {
		Blockchain = newBlockchain
	} else {
		log.Println("INVALID CHAIN!!!")
	}
	return nil
}

func isChainValid(bs []Block) bool {
	for i := len(bs) - 1; i > 0; i-- {
		if !isBlockValid(bs[i], bs[i-1]) {
			return false
		}
	}
	return true
}
