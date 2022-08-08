package main

import (
	"context"
	"log"
	"math/rand"
	"net"
	"os"
	"time"

	pb "gozab/gozab"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type Ack struct {
	valid   bool
	serial  int32
	epoch   int32
	counter int32
}

var (
	serverNum   = 5
	userPort    = "localhost:50056"
	serverPorts = []string{"localhost:50051", "localhost:50052", "localhost:50053", "localhost:50054", "localhost:50055"}

	propBuffers   [5]chan *pb.PropTxn
	commitBuffers [5]chan *pb.CommitTxn
	ackBuffer     chan Ack // size 5 queue
	beatBuffer    chan int // size 5 buffer

	upFollowers = []bool{true, true, true, true, true}
	upNum       = serverNum

	// for leader convenience only
	lastEpoch int32 = 0
	lastCount int32 = 0

	// for follower use
	// TODO: remember to update these
	lastEpochProp  int32 = 0
	lastLeaderProp int32 = 0

	archive  []*pb.PropTxn
	pStorage []*pb.PropTxn
	dStruct  map[string]int32

	voted                   chan int32
	voteRequestResultBuffer chan stateEpoch // size 5 buffer
	currentLeader           string          // change all the for loops
)

type leaderServer struct {
	pb.UnimplementedLeaderUserServer
}

type followerServer struct {
	pb.UnimplementedFollowerLeaderServer
}

type voterServer struct {
	pb.UnimplementedVoterCandidateServer
}

type stateEpoch struct {
	state bool
	epoch int32
}

type stateHist struct {
	state bool
	hist  []*pb.PropTxn
}

func main() {
	archive = make([]*pb.PropTxn, 0)
	pStorage = make([]*pb.PropTxn, 0) // TODO: merge current pStorage into archive and declare new pStorage
	dStruct = make(map[string]int32)
	if os.Args[1] == "lead" {
		LeaderRoutine(os.Args[2])
	} else if os.Args[1] == "follow" {
		FollowerRoutine(os.Args[2])
	}
	// default: election
	log.Printf("entering election...")
	mainHolder := make(chan int) // prevent main from exiting
	ElectionRoutine(os.Args[2])
	<-mainHolder
}

// Leader: implementation of user Store handler
func (s *leaderServer) Store(ctx context.Context, in *pb.Vec) (*pb.Empty, error) {
	log.Printf("Leader received user request\n")
	for i := 0; i < serverNum; i++ {
		propBuffers[i] <- &pb.PropTxn{E: lastEpoch, Transaction: &pb.Txn{V: &pb.Vec{Key: in.GetKey(), Value: in.GetValue()}, Z: &pb.Zxid{Epoch: lastEpoch, Counter: lastCount}}}
	}
	lastCount++
	return &pb.Empty{Content: "Leader recieved your request"}, nil
}

// Leader: implementation of user Retrieve handler
func (s *leaderServer) Retrieve(ctx context.Context, in *pb.GetTxn) (*pb.ResultTxn, error) {
	log.Printf("Leader received user retrieve\n")
	return &pb.ResultTxn{Value: dStruct[in.GetKey()]}, nil
}

func LeaderRoutine(port string) {
	for i := 0; i < serverNum; i++ {
		propBuffers[i] = make(chan *pb.PropTxn)
		commitBuffers[i] = make(chan *pb.CommitTxn)
	}
	ackBuffer = make(chan Ack, 5)

	go FollowerRoutine(port)

	messengerStat := make(chan string)

	go MessengerRoutine(serverPorts[0], 0, messengerStat)
	go MessengerRoutine(serverPorts[1], 1, messengerStat)
	go MessengerRoutine(serverPorts[2], 2, messengerStat)
	go MessengerRoutine(serverPorts[3], 3, messengerStat)
	go MessengerRoutine(serverPorts[4], 4, messengerStat)

	go AckToCmtRoutine()

	go registerL()

	<-messengerStat
	log.Printf("quorum dead")
}

func registerL() {
	// listen user
	lis, err := net.Listen("tcp", userPort)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterLeaderUserServer(s, &leaderServer{})
	log.Printf("leader listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

// Voter: implementation of AskVote handler
func (s *voterServer) AskVote(ctx context.Context, in *pb.Epoch) (*pb.Vote, error) {
	log.Printf("Leader received user retrieve\n")
	select {
	case <-voted:
		return &pb.Vote{Voted: true}, nil
	default:
		return &pb.Vote{Voted: false}, nil
	}

}

// Voter: implementation of ACK-E handler
func (s *voterServer) NewEpoch(ctx context.Context, in *pb.Epoch) (*pb.ACK_E, error) {
	// check new epoch
	if lastEpochProp >= in.GetEpoch() {
		return nil, status.Errorf(codes.InvalidArgument,
			"new epoch not new enough")
	}

	// acknowledge new epoch proposal
	return &pb.ACK_E{LastLeaderProp: lastLeaderProp, Hist: pStorage}, nil
}

func ElectionRoutine(port string) {
	voted = make(chan int32)
	voted <- 0
	go registerV(port)

	rand.Seed(time.Now().UnixNano())
	n := rand.Intn(10) // n will be between 0 and 10

	time.Sleep(time.Duration(n) * time.Millisecond)
	// time to check
	select {
	// didn't receive request, try to be leader
	case <-voted:

		// initialize routines
		voteCount := 0
		ackeCount := 0
		voteRequestResultBuffer = make(chan stateEpoch, 5)
		electionHolder := make(chan stateEpoch, 5)
		synchronizationHolder := make(chan stateHist, 5)
		ackeResultBuffer := make(chan stateHist, 5)
		go ElectionMessengerRoutine(serverPorts[0], electionHolder, synchronizationHolder, ackeResultBuffer)
		go ElectionMessengerRoutine(serverPorts[1], electionHolder, synchronizationHolder, ackeResultBuffer)
		go ElectionMessengerRoutine(serverPorts[2], electionHolder, synchronizationHolder, ackeResultBuffer)
		go ElectionMessengerRoutine(serverPorts[3], electionHolder, synchronizationHolder, ackeResultBuffer)
		go ElectionMessengerRoutine(serverPorts[4], electionHolder, synchronizationHolder, ackeResultBuffer)

		// check vote request results
		var latestE int32 = -1
		for i := 0; i < 5; i++ {
			result := <-voteRequestResultBuffer
			if result.state {
				voteCount++
				if result.epoch > latestE {
					latestE = result.epoch
				}
			}
		}
		lastEpoch = latestE + 1
		if voteCount <= serverNum/2 {
			// cancel ElectionMessengerRoutine, restart election
			for i := 0; i < 5; i++ {
				electionHolder <- stateEpoch{false, -1}
			}
			go ElectionRoutine(port)
			return
		} else {
			// let ElectionMessengerRoutines proceed
			for i := 0; i < 5; i++ {
				electionHolder <- stateEpoch{true, lastEpoch}
			}
		}

		// check ACK-E results
		var latestHist = []*pb.PropTxn{{E: -1, Transaction: &pb.Txn{V: &pb.Vec{Key: "", Value: -1}, Z: &pb.Zxid{Epoch: -1, Counter: -1}}}}
		for i := 0; i < 5; i++ {
			result := <-ackeResultBuffer
			if result.state {
				ackeCount++
				if result.hist[len(result.hist)-1].E > latestHist[len(latestHist)].E || result.hist[len(result.hist)-1].E == latestHist[len(latestHist)].E && result.hist[len(result.hist)-1].Transaction.Z.Counter >= latestHist[len(latestHist)].Transaction.Z.Counter {
					latestHist = result.hist
				}
			}
		}
		if ackeCount <= serverNum/2 {
			// restart election
			for i := 0; i < 5; i++ {
				synchronizationHolder <- stateHist{false, []*pb.PropTxn{{E: -1, Transaction: &pb.Txn{V: &pb.Vec{Key: "", Value: -1}, Z: &pb.Zxid{Epoch: -1, Counter: -1}}}}}
			}
			go ElectionRoutine(port)
			return
		} else {
			// let ElectionMessengerRoutines proceed
			for i := 0; i < 5; i++ {
				synchronizationHolder <- stateHist{true, latestHist}
			}
		}

		// become prospective leader
		// TODO: synchronization
	default:
		// become follower
		// TODO: synchronization
	}
}

func ElectionMessengerRoutine(port string, electionHolder chan stateEpoch, synchronizationHolder chan stateHist, ackeResultBuffer chan stateHist) {
	// dial and askvote
	conn, err := grpc.Dial(port, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("could not connect to server %s: %v", port, err)
	}
	defer conn.Close()

	client := pb.NewVoterCandidateClient(conn)

	askvoteHelper(port, client)

	// wait for quorum check
	check := <-electionHolder
	if !check.state {
		return
	}
	// proceed, send new epoch
	newepochHelper(port, check, ackeResultBuffer, client)
}

func askvoteHelper(port string, client pb.VoterCandidateClient) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := client.AskVote(ctx, &pb.Epoch{Epoch: lastEpochProp})
	if r != nil {
		voteRequestResultBuffer <- stateEpoch{false, -1}
		log.Printf("failed to ask vote from %s: %v", port, err)
		return
	}
	if r.GetVoted() {
		voteRequestResultBuffer <- stateEpoch{true, lastEpochProp}
	}
}

func newepochHelper(port string, check stateEpoch, ackeResultBuffer chan stateHist, client pb.VoterCandidateClient) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hist, err := client.NewEpoch(ctx, &pb.Epoch{Epoch: check.epoch})
	if err != nil {
		// this messenger is dead
		ackeResultBuffer <- stateHist{false, nil}
		log.Printf("messenger quit: failed to get ACK-E from %s: %v", port, err)
		return
	}
	ackeResultBuffer <- stateHist{true, hist.GetHist()}
}

func registerV(port string) {
	// listen port
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterVoterCandidateServer(s, &voterServer{})
	log.Printf("voter listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

// Follower: implementation of Broadcast handler
func (s *followerServer) Broadcast(ctx context.Context, in *pb.PropTxn) (*pb.AckTxn, error) {
	log.Printf("Follower received proposal\n")
	// writes the proposal to local stable storage
	pStorage = append(pStorage, in)
	log.Printf("local stable storage: %+v\n", pStorage)
	return &pb.AckTxn{Content: "I Acknowledged"}, nil
}

// Follower: implementation of Commit handler
func (s *followerServer) Commit(ctx context.Context, in *pb.CommitTxn) (*pb.Empty, error) {
	log.Printf("Follower received commit\n")
	// writes the transaction from local stable storage to local data structure
	dStruct[pStorage[len(pStorage)-1].Transaction.V.Key] = pStorage[len(pStorage)-1].Transaction.V.Value
	log.Printf("local data structure: %+v\n", dStruct)
	return &pb.Empty{Content: "Commit message recieved"}, nil
}

// Follower: implementation of HeartBeat handler
func (s *followerServer) HeartBeat(ctx context.Context, in *pb.Empty) (*pb.Empty, error) {
	beatBuffer <- 1
	return &pb.Empty{Content: "bump"}, nil
}

func FollowerRoutine(port string) {
	// follower receiving leader's heartbeat
	beatBuffer = make(chan int, 5)
	leaderStat := make(chan string)
	go BeatReceiver(leaderStat)

	go registerF(port)

	<-leaderStat
	log.Printf("leader dead")
}

func registerF(port string) {
	// listen leader on port
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterFollowerLeaderServer(s, &followerServer{})
	log.Printf("server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

func AckToCmtRoutine() {
	for {
		// Collect acknowledgements from Messengers
		// Update upFollowers statistics
		for i := 0; i < serverNum; i++ {
			ack := <-ackBuffer
			if !ack.valid && upFollowers[ack.serial] {
				upFollowers[ack.serial] = false
				upNum--
			}
		}

		// Check if quorum dead
		if upNum <= serverNum/2 {
			log.Printf("quorum dead")
			return
		}

		// Send commits to the acknowledged followers
		for i, acknowledged := range upFollowers {
			if acknowledged {
				commitBuffers[i] <- &pb.CommitTxn{Content: "Please commit"}
			}
		}
	}
}

// CORE BROACAST FUNCTION!
func MessengerRoutine(port string, serial int32, messengerStat chan string) {
	// dial follower
	conn, err := grpc.Dial(serverPorts[serial], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("could not connect to server %s: %v", port, err)
	}
	defer conn.Close()

	client := pb.NewFollowerLeaderClient(conn)

	// leader receiving follower's heartbeat
	followerStat := make(chan string)
	go BeatSender(client, followerStat)

	go propcmt(client, port, serial)

	<-followerStat
	upFollowers[serial] = false
	upNum--
	log.Printf("follower %s down, messenger quit", port)
	// check quorum dead
	if upNum <= serverNum/2 {
		messengerStat <- "returned"
	}
}

func propcmt(client pb.FollowerLeaderClient, port string, serial int32) {
	for {
		prop(client, port, serial)
		cmt(client, port, serial)
	}
}

func prop(client pb.FollowerLeaderClient, port string, serial int32) {
	// send proposal
	proposal := <-propBuffers[serial]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	r, err := client.Broadcast(ctx, proposal)
	cancel() // defer?
	if err != nil {
		log.Printf("could not broadcast to server %s: %v", port, err)
		status, ok := status.FromError(err)
		if ok {
			if status.Code() == codes.DeadlineExceeded {
				log.Printf("Server %s timeout, Messenger exit", port)
			}
		}
		// dead Messenger
		log.Printf("messenger on %s initiated death mode", port)
		ackBuffer <- Ack{false, serial, -1, -1}
		for {
			<-propBuffers[serial]
			ackBuffer <- Ack{false, serial, -1, -1}
		}
	}
	if r.GetContent() == "I Acknowledged" {
		ackBuffer <- Ack{true, serial, proposal.GetTransaction().GetZ().Epoch, proposal.GetTransaction().GetZ().Counter}
	}
}

func cmt(client pb.FollowerLeaderClient, port string, serial int32) {
	// send commit
	commit := <-commitBuffers[serial]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	r, err := client.Commit(ctx, commit)
	cancel() // defer?
	if err != nil {
		log.Printf("could not issue commit to server %s: %v", port, err)
		status, ok := status.FromError(err)
		if ok {
			if status.Code() == codes.DeadlineExceeded {
				log.Printf("Server %s timeout, Messenger exit", port)
			}
		}
		// dead Messenger
		log.Printf("messenger on %s initiated death mode", port)
		for {
			<-propBuffers[serial]
			ackBuffer <- Ack{false, serial, -1, -1}
		}
	}
	if r.GetContent() == "Commit message recieved" {
		log.Printf("Commit feedback recieved from %s", port)
	}
}

func BeatSender(client pb.FollowerLeaderClient, followerStat chan string) {
	for {
		time.Sleep(100 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := client.HeartBeat(ctx, &pb.Empty{Content: "Beat"})
		cancel() // defer?
		if err != nil {
			followerStat <- "dead"
			return
		}
	}
}

func BeatReceiver(leaderStat chan string) {
	for {
		select {
		case <-beatBuffer:
			log.Printf("beat")
		case <-time.After(5 * time.Second):
			leaderStat <- "dead"
			return
		}
	}
}
