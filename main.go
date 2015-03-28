package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/etcdserver/stats"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/rafthttp"
)

func main() {
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	cluster := flag.String("cluster", "http://127.0.0.1:9021", "")
	id := flag.Int("id", 1, "")
	flag.Parse()

	peers := strings.Split(*cluster, ",")

	ready := make(chan struct{})
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		ready <- struct{}{}
	})
	go func() {
		log.Printf("server http on %v", peers[*id-1][len("http://"):])
		log.Fatal(http.ListenAndServe(peers[*id-1][len("http://"):], nil))
	}()

	for i := 1; i <= len(peers); i++ {
		if *id != i {
			go func(j int) {
				for {
					// TODO: fast timeout
					resp, err := http.Get(peers[j-1] + "/ready")
					if err != nil {
						continue
					}
					resp.Body.Close()
					return
				}
			}(i)
		}
	}

	for i := 0; i < len(peers)-1; i++ {
		<-ready
		fmt.Printf("%d peers are ready for benchmark\n", 2+i)
	}

	fmt.Println("starting benchmark...")
	time.Sleep(5 * time.Second)

	rn := setup(*id, peers)
	go rn.run()

	var r etcdserverpb.Request
	r.Method = "PUT"
	r.Path = "/foo/bar"
	r.Val = "zar"
	data, _ := r.Marshal()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
	}
	now := time.Now()
	if *id == 1 {
		for i := 0; i < 500000; i++ {
			rn.Propose(context.TODO(), data)
		}
	}
	<-rn.reach
	pprof.StopCPUProfile()
	d := time.Since(now)
	fmt.Printf("throughput: %d ops per second\n", uint64(500000*time.Second/d))
	time.Sleep(time.Second * 2)
}

func setup(id int, peers []string) *raftNode {
	rpeers := make([]raft.Peer, len(peers))
	for i := 1; i <= len(rpeers); i++ {
		rpeers[i-1] = raft.Peer{ID: uint64(i)}
	}

	log.Printf("setup cluster %v: %s", rpeers, peers)
	log.Printf("setup node %d: %s", id, peers[id-1])

	s := raft.NewMemoryStorage()
	c := &raft.Config{
		ID:              uint64(id),
		ElectionTick:    10,
		HeartbeatTick:   1,
		Storage:         s,
		MaxSizePerMsg:   1024 * 1024,
		MaxInflightMsgs: 256,
	}
	node := raft.StartNode(c, rpeers)

	rn := &raftNode{
		Node:        node,
		raftStorage: s,
		ticker:      time.NewTicker(tickDuration).C,
		trans:       &noopTrans{},
		goal:        500002,
		reach:       make(chan struct{}),
		done:        make(chan struct{}),
	}

	ss := &stats.ServerStats{}
	ss.Initialize()
	ls := stats.NewLeaderStats("dummy")
	tr := rafthttp.NewTransporter(&http.Transport{}, types.ID(id), types.ID(0x1000), rn, nil, ss, ls)
	for i := 1; i <= len(peers); i++ {
		if i != id {
			tr.AddPeer(types.ID(i), []string{peers[i-1]})
		}
	}

	http.Handle("/raft", tr.Handler())
	http.Handle("/raft/", tr.Handler())
	rn.trans = tr

	return rn
}