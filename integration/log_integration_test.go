//+build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/google/trillian"
	"github.com/google/trillian/crypto"
	"github.com/google/trillian/merkle"
	"github.com/google/trillian/storage/tools"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var serverFlag = flag.String("log_server", "localhost:8092", "Server address:port")
var queueLeavesFlag = flag.Bool("queue_leaves", true, "If true queues leaves, false just reads from the log")
var awaitSequencingFlag = flag.Bool("await_sequencing", true, "If true then waits until log size is at least num_leaves")
var checkLogEmptyFlag = flag.Bool("check_log_empty", true, "If true ensures log is empty before queuing anything")
var startLeafFlag = flag.Int64("start_leaf", 0, "The first leaf index to use")
var numLeavesFlag = flag.Int64("num_leaves", 1000, "The number of leaves to submit and read back")
var queueBatchSizeFlag = flag.Int("queue_batch_size", 50, "Batch size when queueing leaves")
var readBatchSizeFlag = flag.Int64("read_batch_size", 50, "Batch size when getting leaves by index")
var waitForSequencingFlag = flag.Duration("wait_for_sequencing", time.Second * 60, "How long to wait for leaves to be sequenced")
var waitBetweenQueueChecksFlag = flag.Duration("queue_poll_wait", time.Second * 5, "How frequently to check the queue while waiting")
var rpcRequestDeadlineFlag = flag.Duration("rpc_deadline", time.Second * 10, "Deadline to use for all RPC requests")

// testParameters bundles up all the settings for a test run
type testParameters struct {
	startLeaf           int64
	leafCount           int64
	queueBatchSize      int
	readBatchSize       int64
	sequencingWaitTotal time.Duration
	sequencingPollWait  time.Duration
}

func TestLogIntegration(t *testing.T) {
	flag.Parse()

	// Step 0 - Initialize and connect to log server
	treeID := tools.GetLogIDFromFlagsOrDie()
	params := testParameters{startLeaf: *startLeafFlag, leafCount: *numLeavesFlag, queueBatchSize: *queueBatchSizeFlag, readBatchSize: *readBatchSizeFlag, sequencingWaitTotal: *waitForSequencingFlag, sequencingPollWait: *waitBetweenQueueChecksFlag}

	if params.startLeaf < 0 || params.leafCount <= 0 {
		t.Fatalf("Start leaf index must be >= 0 (%d) and number of leaves must be > 0 (%d)", params.startLeaf, params.leafCount)
	}

	// TODO: Other options apart from insecure connections
	conn, err := grpc.Dial(*serverFlag, grpc.WithInsecure(), grpc.WithTimeout(time.Second * 5))

	if err != nil {
		t.Fatalf("Failed to connect to log server: %v", err)
	}

	defer conn.Close()

	client := trillian.NewTrillianLogClient(conn)

	// Step 1 - Optionally check log starts empty then optionally queue leaves on server
	if *checkLogEmptyFlag {
		glog.Infof("Checking log is empty before starting test")
		resp, err := getLatestSignedLogRoot(client, treeID)

		if err != nil || resp.Status.StatusCode != trillian.TrillianApiStatusCode_OK {
			t.Fatalf("Failed to get latest log root: %v %v", resp, err)
		}

		if resp.SignedLogRoot.TreeSize > 0 {
			t.Fatalf("Expected an empty log but got tree head response: %v", resp)
		}
	}

	if *queueLeavesFlag {
		glog.Infof("Queueing %d leaves to log server ...", params.leafCount)
		if err := queueLeaves(treeID, client, params); err != nil {
			t.Fatalf("Failed to queue leaves: %v", err)
		}
	}

	// Step 2 - Wait for queue to drain when server sequences, give up if it doesn't happen (optional)
	if *awaitSequencingFlag {
		glog.Infof("Waiting for log to sequence ...")
		if err := waitForSequencing(treeID, client, params); err != nil {
			t.Fatalf("Leaves were not sequenced: %v", err)
		}
	}

	// Step 3 - Use get entries to read back what was written, check leaves are correct
	glog.Infof("Reading back leaves from log ...")
	leafMap, err := readbackLogEntries(treeID, client, params)

	if err != nil {
		t.Fatalf("Could not read back log entries: %v", err)
	}

	// Step 4 - Cross validation between log and memory tree root hashes
	glog.Infof("Checking log STH with our constructed in-memory tree ...")
	tree := buildMemoryMerkleTree(leafMap, params)
	if err := checkLogRootHashMatches(treeID, tree, client, params); err != nil {
		t.Fatalf("Log consistency check failed: %v", err)
	}
}

func queueLeaves(treeID int64, client trillian.TrillianLogClient, params testParameters) error {
	leaves := []trillian.LogLeaf{}

	for l := int64(0); l < params.leafCount; l++ {
		// Leaf data based on the sequence number so we can check the hashes
		leafNumber := params.startLeaf + l

		data := []byte(fmt.Sprintf("Leaf %d", leafNumber))
		hash := sha256.Sum256(data)

		leaf := trillian.LogLeaf{
			// TODO(Martin2112): This should be LeafValueHash but it doesn't exist yet
			MerkleLeafHash: hash[:],
			LeafValue:      data,
			ExtraData:      nil,
			LeafIndex:      0}
		leaves = append(leaves, leaf)

		if len(leaves) >= params.queueBatchSize || (l + 1) == params.leafCount {
			glog.Infof("Queueing %d leaves ...", len(leaves))

			req := makeQueueLeavesRequest(treeID, leaves)
			ctx, cancelFunc := getRPCDeadlineContext()
			response, err := client.QueueLeaves(ctx, &req)
			cancelFunc()

			if err != nil {
				return err
			}

			if got := response.Status; got == nil || got.StatusCode != trillian.TrillianApiStatusCode_OK {
				return fmt.Errorf("queue leaves failed: %s %d", response.Status.Description, response.Status.StatusCode)
			}

			leaves = leaves[:0] // starting new batch
		}
	}

	return nil
}

func waitForSequencing(treeID int64, client trillian.TrillianLogClient, params testParameters) error {
	endTime := time.Now().Add(params.sequencingWaitTotal)

	glog.Infof("Waiting for sequencing until: %v", endTime)

	for endTime.After(time.Now()) {
		req := trillian.GetSequencedLeafCountRequest{LogId: treeID}
		ctx, cancelFunc := getRPCDeadlineContext()
		sequencedLeaves, err := client.GetSequencedLeafCount(ctx, &req)
		cancelFunc()

		if err != nil {
			return err
		}

		glog.Infof("Leaf count: %d", sequencedLeaves.LeafCount)

		if sequencedLeaves.LeafCount >= params.leafCount + params.startLeaf {
			return nil
		}

		glog.Infof("Leaves sequenced: %d. Still waiting ...", sequencedLeaves.LeafCount)

		time.Sleep(params.sequencingPollWait)
	}

	return errors.New("wait time expired")
}

func readbackLogEntries(logID int64, client trillian.TrillianLogClient, params testParameters) (map[int64]*trillian.LogLeaf, error) {
	currentLeaf := int64(0)
	leafMap := make(map[int64]*trillian.LogLeaf)

	// Build a map of all the leaf data we expect to have seen when we've read all the leaves.
	// Have to work with strings because slices can't be map keys. Sigh.
	leafDataPresenceMap := make(map[string]bool)

	for l := int64(0); l < params.leafCount; l++ {
		leafDataPresenceMap[fmt.Sprintf("Leaf %d", l + params.startLeaf)] = true
	}

	for currentLeaf < params.leafCount {
		hasher := merkle.NewRFC6962TreeHasher(crypto.NewSHA256())

		// We have to allow for the last batch potentially being a short one
		numLeaves := params.leafCount - currentLeaf

		if numLeaves > params.readBatchSize {
			numLeaves = params.readBatchSize
		}

		glog.Infof("Reading %d leaves from %d ...", numLeaves, currentLeaf + params.startLeaf)
		req := makeGetLeavesByIndexRequest(logID, currentLeaf + params.startLeaf, numLeaves)
		ctx, cancelFunc := getRPCDeadlineContext()
		response, err := client.GetLeavesByIndex(ctx, req)
		cancelFunc()

		if err != nil {
			return nil, err
		}

		if got := response.Status; got == nil || got.StatusCode != trillian.TrillianApiStatusCode_OK {
			return nil, fmt.Errorf("read leaves failed: %s %d", response.Status.Description, response.Status.StatusCode)
		}

		// Check we got the right leaf count
		if len(response.Leaves) == 0 {
			return nil, fmt.Errorf("expected %d leaves log returned none", numLeaves)
		}

		// Check the leaf contents make sense. Can't rely on exact ordering as queue timestamps will be
		// close between batches and identical within batches.
		for l := 0; l < len(response.Leaves); l++ {
			// Check for duplicate leaf index in response data - should not happen
			leaf := response.Leaves[l]

			if _, ok := leafMap[leaf.LeafIndex]; ok {
				return nil, fmt.Errorf("got duplicate leaf sequence number: %d", leaf.LeafIndex)
			}

			leafMap[leaf.LeafIndex] = leaf

			// Test for having seen duplicate leaf data - it should all be distinct
			_, ok := leafDataPresenceMap[string(leaf.LeafValue)]

			if !ok {
				return nil, fmt.Errorf("leaf data duplicated for leaf: %v", leaf)
			}

			delete(leafDataPresenceMap, string(leaf.LeafValue))

			hash := hasher.HashLeaf(response.Leaves[l].LeafValue)

			if got, want := base64.StdEncoding.EncodeToString(hash), base64.StdEncoding.EncodeToString(leaf.MerkleLeafHash); !bytes.Equal(hash[:], leaf.MerkleLeafHash) {
				return nil, fmt.Errorf("leaf hash mismatch expected got: %s want: %s", got, want)
			}
		}

		currentLeaf += int64(len(response.Leaves))
	}

	// By this point we expect to have seen all the leaves so there should be nothing in the map
	if len(leafDataPresenceMap) != 0 {
		return nil, fmt.Errorf("missing leaves from data read back: %v", leafDataPresenceMap)
	}

	return leafMap, nil
}

func checkLogRootHashMatches(logID int64, tree *merkle.InMemoryMerkleTree, client trillian.TrillianLogClient, params testParameters) error {
	// Check the STH against the hash we got from our tree
	resp, err := getLatestSignedLogRoot(client, logID)

	if err != nil {
		return err
	}

	// Hash must not be empty and must match the one we built ourselves
	if got, want := base64.StdEncoding.EncodeToString(resp.SignedLogRoot.RootHash), base64.StdEncoding.EncodeToString(tree.CurrentRoot().Hash()); got != want {
		return fmt.Errorf("root hash mismatch expected got: %s want: %s", got, want)
	}

	return nil
}

func makeQueueLeavesRequest(logID int64, leaves []trillian.LogLeaf) trillian.QueueLeavesRequest {
	leafProtos := make([]*trillian.LogLeaf, 0, len(leaves))

	for _, leaf := range leaves {
		// TODO(Martin2112): This should be using the leaf value hash but it's not there yet
		proto := trillian.LogLeaf{LeafIndex: leaf.LeafIndex, MerkleLeafHash: leaf.MerkleLeafHash, LeafValue: leaf.LeafValue, ExtraData: leaf.ExtraData}
		leafProtos = append(leafProtos, &proto)
	}

	return trillian.QueueLeavesRequest{LogId: logID, Leaves: leafProtos}
}

func makeGetLeavesByIndexRequest(logID int64, startLeaf, numLeaves int64) *trillian.GetLeavesByIndexRequest {
	leafIndices := make([]int64, 0, numLeaves)

	for l := int64(0); l < numLeaves; l++ {
		leafIndices = append(leafIndices, l + startLeaf)
	}

	return &trillian.GetLeavesByIndexRequest{LogId: logID, LeafIndex: leafIndices}
}

func buildMemoryMerkleTree(leafMap map[int64]*trillian.LogLeaf, params testParameters) *merkle.InMemoryMerkleTree {
	// Build the same tree with two different merkle implementations as an additional check. We don't
	// just rely on the compact tree as the server uses the same code so bugs could be masked
	compactTree := merkle.NewCompactMerkleTree(merkle.NewRFC6962TreeHasher(crypto.NewSHA256()))
	merkleTree := merkle.NewInMemoryMerkleTree(merkle.NewRFC6962TreeHasher(crypto.NewSHA256()))

	// We use the leafMap as we need to use the same order for the memory tree to get the same hash.
	for l := params.startLeaf; l < params.leafCount; l++ {
		compactTree.AddLeaf(leafMap[l].LeafValue, func(depth int, index int64, hash []byte) {})
		merkleTree.AddLeaf(leafMap[l].LeafValue)
	}

	// If the two reference results disagree there's no point in continuing the checks. This is a
	// "can't happen" situation.
	if !bytes.Equal(compactTree.CurrentRoot(), merkleTree.CurrentRoot().Hash()) {
		glog.Fatalf("different root hash results from merkle tree building: %v and %v", compactTree.CurrentRoot(), merkleTree.CurrentRoot())
	}

	return merkleTree
}

func getLatestSignedLogRoot(client trillian.TrillianLogClient, logID int64) (*trillian.GetLatestSignedLogRootResponse, error) {
	req := trillian.GetLatestSignedLogRootRequest{LogId: logID}
	ctx, cancelFunc := getRPCDeadlineContext()
	resp, err := client.GetLatestSignedLogRoot(ctx, &req)
	cancelFunc()

	return resp, err
}

// getRPCDeadlineTime calculates the future time an RPC should expire based on our config
func getRPCDeadlineContext() (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), time.Now().Add(*rpcRequestDeadlineFlag))
}
