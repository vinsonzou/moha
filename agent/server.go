// Copyright 2018 MOBIKE, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"git.mobike.io/database/mysql-agent/pkg/log"
	"git.mobike.io/database/mysql-agent/pkg/mysql"
	"git.mobike.io/database/mysql-agent/pkg/systemcall"
	"github.com/juju/errors"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// latestPos records the latest MySQL binlog of the agent works on.
	latestPos Position

	fetchBinlogInterval = 1 * time.Second

	// leaderValue stores the value that will be put to leaderpath
	// if CURRENT LeaderFollowerRegistry is the leader
	leaderValue string
)

func init() {
	latestPos.SecondsBehindMaster = 1 << 30
}

// Server maintains agent's status at runtime.
type Server struct {
	ctx    context.Context
	cancel context.CancelFunc

	// node maintains the status of MySQL and interact with etcd registry.
	node Node

	serviceManager ServiceManager

	isLeader   int32
	onlyFollow bool

	// leaseExpireTS is the timestamp that lease might expire if lease is failed to renew.
	// leaseExpireTS := time.Now() + leaseTTL - shutdownThreshold
	leaseExpireTS int64

	fdStopCh chan interface{}

	isClosed     int32
	leaderStopCh chan interface{}
	httpSrv      *http.Server

	shutdownCh chan interface{}

	//TODO term increment check
	term     uint64
	lastUUID string
	lastGTID string

	// uuid is the uuid of current MySQL
	uuid       string
	uuidRWLock sync.RWMutex

	db *sql.DB

	cfg *Config
}

func checkConfig(cfg *Config) error {
	if cfg == nil {
		return errors.NotValidf("cfg is nil")
	}

	if cfg.DataDir == "" {
		return errors.NotValidf("DataDir is empty")
	}

	if cfg.EtcdRootPath == "" {
		return errors.NotValidf("EtcdRootPath is empty")
	}

	if cfg.LeaderLeaseTTL == 0 {
		return errors.NotValidf("LeaderLeaseTTL is 0")
	}

	if cfg.ShutdownThreshold == 0 {
		return errors.NotValidf("ShutdownThreshold is 0")
	}

	if cfg.RegisterTTL == 0 {
		return errors.NotValidf("RegisterTTL is 0")
	}

	if cfg.EtcdUsername == "" {
		return errors.NotValidf("Etcd Username is empty")
	}
	return nil
}

// NewServer returns an instance of agent server.
func NewServer(cfg *Config) (*Server, error) {

	err := checkConfig(cfg)
	if err != nil {
		log.Error("cfg is illegal, err: ", err)
		return nil, err
	}

	n, err := NewAgentNode(cfg)
	if err != nil {
		return nil, errors.Trace(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		ctx:        ctx,
		cancel:     cancel,
		node:       n,
		cfg:        cfg,
		shutdownCh: make(chan interface{}),
	}, nil
}

// Start runs Server, and maintains heartbeat to etcd.
func (s *Server) Start() error {
	// mark started
	atomic.StoreInt32(&s.isClosed, 0)
	initMetrics()
	leaderValue = s.node.ID()
	go s.startWriteFDLoop()

	// init the HTTP server.
	httpSvr, err := s.initHTTPServer()
	if err != nil {
		return err
	}
	s.httpSrv = httpSvr
	// run HTTP Server
	go s.httpSrv.ListenAndServe()

	// try to start(double fork and exec) a new mysql if current mysql is down
	if !isPortAlive(s.cfg.DBConfig.Host, fmt.Sprint(s.cfg.DBConfig.Port)) {
		log.Info("config file is ", s.cfg.configFile)
		log.Info("mysql is not alive, try to start")
		log.Info("double fork and exec ", s.cfg.ForkProcessFile, s.cfg.ForkProcessArgs)
		err := systemcall.DoubleForkAndExecute(s.cfg.configFile)
		if err != nil {
			log.Error("error while double fork ", s.cfg.ForkProcessFile, " error is ", err)
			return errors.Trace(err)
		}
		stopCh := make(chan interface{})
		select {
		case <-waitUtilPortAlive(s.cfg.DBConfig.Host, fmt.Sprint(s.cfg.DBConfig.Port), stopCh):
			log.Info("mysql has been started")
			break
		case <-time.After(time.Duration(s.cfg.ForkProcessWaitSecond) * time.Second):
			stopCh <- "timeout"
			return errors.New("cannot start mysql, timeout")
		}
	} else {
		log.Info("mysql is alive, according to port alive detection")
	}
	log.Info("mysql fork done, try to connect")

	// try to connect MySQL
	var db *sql.DB
	startTime := time.Now()
	for true {
		if time.Now().Sub(startTime) > time.Duration(s.cfg.ForkProcessWaitSecond)*time.Second {
			log.Errorf("timeout to connect to MySQL by user %s", s.cfg.DBConfig.User)
			return errors.Errorf("timeout to connect to MySQL by user %s", s.cfg.DBConfig.User)
		}
		db, err = mysql.CreateDB(s.cfg.DBConfig)
		if err != nil {
			log.Errorf("fail to connect MySQL in agent start: %+v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		err = mysql.Select1(db)
		if err != nil {
			log.Errorf("fail to select 1 from MySQL in agent start: %+v", err)
			db.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	log.Info("db is successfully connected and select 1 is OK")
	s.db = db

	// init ServiceManager
	sm := &mysqlServiceManager{
		db:                  s.db,
		mysqlNet:            s.cfg.DBConfig.ReplicationNet,
		replicationUser:     s.cfg.DBConfig.ReplicationUser,
		replicationPassword: s.cfg.DBConfig.ReplicationPassword,
	}
	s.serviceManager = sm

	// before register this node, set service to readonly first
	for true {
		err = s.serviceManager.SetReadOnly()
		if err == nil {
			break
		}
		log.Errorf("has error in SetReadOnly. self spin. err: %+v ", err)
		time.Sleep(100 * time.Millisecond)
	}
	// load the executed gtid
	s.loadUUID()

	// start heartbeat loop.
	go s.startHeartbeatLoop()
	// start monitor binlog goroutine.
	go s.startBinlogMonitorLoop()
	go s.leaderLoop()

	// now init nearly done, begin to write to fd
	s.writeFD()

	<-s.shutdownCh
	log.Info("receive from s.shutdownCh, exit Start() function")
	return nil

}

// Close gracefully releases resource of agent server.
func (s *Server) Close() {
	// mark closed
	if !atomic.CompareAndSwapInt32(&s.isClosed, 0, 1) {
		log.Info("isClose has already been set to 1, another goroutine is closing the server.")
		return
	}

	// lock service to avoid brain-split
	s.setServiceReadonlyOrShutdown()
	s.serviceManager.Close()

	// un-register this node.
	if err := s.node.Unregister(s.ctx); err != nil {
		log.Error(errors.ErrorStack(err))
	}

	// notify other goroutines to exit
	if s.cancel != nil {
		s.cancel()
	}

	// delete leader key
	s.deleteLeaderKey()

	if s.httpSrv != nil {
		s.httpSrv.Shutdown(nil)
	}

	close(s.shutdownCh)
}

func (s *Server) forceClose() {
	os.Exit(3)
}

func (s *Server) initHTTPServer() (*http.Server, error) {
	listenURL, err := url.Parse(s.cfg.ListenAddr)
	if err != nil {
		return nil, errors.Annotate(err, "fail to parse listen address")
	}

	// listen & serve.
	httpSrv := &http.Server{Addr: fmt.Sprintf(":%s", listenURL.Port())}
	// TODO add the new status function to detect all agents' status
	// http.HandleFunc("/status", s.Status)
	http.HandleFunc("/changeMaster", s.ChangeMaster)
	http.HandleFunc("/setReadOnly", s.SetReadOnly)
	http.HandleFunc("/setReadWrite", s.SetReadWrite)
	http.HandleFunc("/setOnlyFollow", s.SetOnlyFollow)
	return httpSrv, nil
}

func (s *Server) writeFD() {
	systemcall.WriteToEventfd(s.cfg.fd, 1)
}

func (s *Server) startWriteFDLoop() {
	// stop write fd goroutine, if there is.
	s.resetWriteFDLoop()
	log.Info("start write fd loop")
	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-s.fdStopCh:
			log.Info("receive sendFDCh, stop sending eventfd")
			ticker.Stop()
			return
		case <-ticker.C:
			s.writeFD()
			agentHeartbeat.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName}).Inc()
		}
	}
}

func (s *Server) resetWriteFDLoop() {
	log.Info("reset write fd loop")
	if s.fdStopCh != nil {
		close(s.fdStopCh)
	}
	s.fdStopCh = make(chan interface{})
}

func (s *Server) startHeartbeatLoop() {
	// TODO wait for starting register and heartbeat
	// invoked by `became master` or `show slave status` has low latency
	for !(s.amILeader() ||
		latestPos.SlaveIORunning && latestPos.SlaveSQLRunning && latestPos.SecondsBehindMaster < 60) {
		log.Infof("s.amILeader(): %t, SecondsBehindMaster: %d", s.amILeader(), latestPos.SecondsBehindMaster)
		time.Sleep(1 * time.Second)
	}
	log.Infof("s.amILeader(): %t, SecondsBehindMaster: %d. so begin register",
		s.amILeader(), latestPos.SecondsBehindMaster)
	// register this node.
	err := s.node.Register(s.ctx)
	for err != nil {
		log.Info("has error when register. ", err)
		time.Sleep(1 * time.Second)
		err = s.node.Register(s.ctx)
	}
	errc := s.node.Heartbeat(s.ctx)
	for err := range errc {
		log.Error("heartbeat has error ", err)
	}
	log.Info("heartbeat is closed")
}

func (s *Server) startBinlogMonitorLoop() {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("monitor loop panic. error: %s, stack: %s", err, debug.Stack())
		}

		s.Close()
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(fetchBinlogInterval):
			err := DoWithRetry(s.updateBinlogPos, "updateBinlogPos", 0, fetchBinlogInterval/2)
			if err != nil {
				log.Warn("has error when updateBinlogPos. err ", err, " ignore.")
			}
		}
	}
}

func (s *Server) updateBinlogPos() error {

	var gtidStr, masterUUID string
	if s.amILeader() {
		pos, gtidSet, err := mysql.GetMasterStatus(s.db)
		if err != nil {
			// error in getting MySQL binlog position
			// TODO change alive status or retry
			return err
		}
		latestPos.File = pos.Name
		latestPos.Pos = fmt.Sprint(pos.Pos)
		latestPos.GTID = gtidSet.String()

		latestPos.SecondsBehindMaster = 0
		latestPos.SlaveIORunning = false
		latestPos.SlaveSQLRunning = false

		// below are monitor logic
		agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "sql_thread"}).Set(0)
		agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "io_thread"}).Set(0)
		gtidStr, masterUUID = latestPos.GTID, latestPos.UUID
	} else {
		rSet, err := mysql.GetSlaveStatus(s.db)
		if err != nil {
			// error in getting MySQL binlog position
			// TODO change alive status or retry
			return errors.Trace(err)
		}
		latestPos.File = rSet["Relay_Master_Log_File"]
		latestPos.Pos = rSet["Exec_Master_Log_Pos"]
		latestPos.GTID = rSet["Executed_Gtid_Set"]

		if rSet["Seconds_Behind_Master"] == "" {
			log.Errorf("Seconds_Behind_Master is empty, show slave status result is %+v", rSet)
		} else {
			sbm, err := strconv.Atoi(rSet["Seconds_Behind_Master"])
			if err != nil {
				log.Warnf("has error in atoi Seconds_Behind_Master: %s. still use previous value: %d. error is %+v",
					rSet["Seconds_Behind_Master"], latestPos.SecondsBehindMaster, err)
			} else {
				latestPos.SecondsBehindMaster = sbm
			}
		}

		if rSet["Slave_SQL_Running"] == "Yes" {
			latestPos.SlaveSQLRunning = true
			agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "sql_thread"}).Set(1)
		} else {
			latestPos.SlaveSQLRunning = false
			agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "sql_thread"}).Set(-1)
		}
		if rSet["Slave_IO_Running"] == "Yes" {
			latestPos.SlaveIORunning = true
			agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "io_thread"}).Set(1)
		} else {
			latestPos.SlaveIORunning = false
			agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "io_thread"}).Set(-1)
		}
		gtidStr, masterUUID = rSet["Executed_Gtid_Set"], rSet["Master_UUID"]
	}

	if masterUUID != "" {
		endTxnID, err := mysql.GetTxnIDFromGTIDStr(gtidStr, masterUUID)
		if err != nil {
			log.Warnf("error when GetTxnIDFromGTIDStr gtidStr: %s, gtid: %s, error is %v", gtidStr, masterUUID, err)
			return nil
		}
		// assume the gtidset is continuous, only pick the last one
		agentSlaveStatus.
			With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "executed_gtid"}).
			Set(float64(endTxnID))
	}

	return nil
}

// leaderLoop includes the leader campaign, master keep alive and slave watch
func (s *Server) leaderLoop() {
	// use defer to catch panic
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("leader loop panic. error: %s, stack: %s", err, debug.Stack())
		}
		s.Close()
	}()

	for {
		//TODO add loop invariant assertion: readonly = 1

		leader, myTerm, err := s.getLeaderAndMyTerm()

		if err != nil {
			isClosed := atomic.LoadInt32(&s.isClosed)
			log.Printf("isClosed: %d", isClosed)
			if isClosed == 1 {
				return
			}
			log.Errorf("get leader err %v", err)
			if errors.IsNotValid(err) {
				log.Errorf("error is failed to validate, mysql-agent is stopped: %+v", err)
				s.Close()
			}
			// TODO for other errors, wait or exit?
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s.term = myTerm

		// if the leader already exists, no need to campaign
		if leader.name != "" {
			if s.isSameLeader(leader.name) {
				// if the leader is the current agent, then we need to keep the lease alive again
				// txn().if(leader is current Agent).then(putWithLease).else()
				// if txn success, set mysql readwrite
				log.Info("current node is still the leader, try to resume leader keepalive")
				s.term = leader.meta.term
				s.lastGTID = leader.meta.lastGTID
				s.lastUUID = leader.meta.lastUUID
				if leader.spm == s.node.ID() {
					// resume single point master mode
					log.Info("leader's spm is current node ID, so resume spm mode")
					s.becomeSinglePointMaster(true)
					continue
				}
				campaignSuccess, lr, err := s.resumeLeader()
				if err != nil {
					log.Errorf("resume leader err %v", err)
					// TODO wait or exit?
					time.Sleep(200 * time.Millisecond)
					continue
				}
				if !campaignSuccess {
					log.Info("fail to resume leader, someone else may be the leader.")
					continue
				}
				// assert: s.leaderStopCh is nil or closed
				s.leaderStopCh = make(chan interface{})
				keepLeaderAliveBarrier := make(chan interface{})
				go func() {
					s.keepLeaderAliveLoop(int64(lr.ID))
					keepLeaderAliveBarrier <- "finish"
				}()

				s.setIsLeaderToTrue()
				err = s.serviceManager.SetReadWrite()
				if err != nil {
					log.Error("fail to set read write, err: ", err)
					s.serviceManager.SetReadOnly()
					close(s.leaderStopCh)
					continue
				}
				<-keepLeaderAliveBarrier

				continue
			} else {
				// now the leader exists and is not current node, therefore just watch
				err = s.preWatch(leader)
				if err != nil {
					log.Error("has error in preWatch, so close.", err)
					s.Close()
					return
				}
				log.Info("current server is no later than master, so watch master")
				log.Infof("leader is %v, watch it", leader)
				err = s.doChangeMaster(leader)
				if err != nil {
					log.Error("error while slave redirects master, goto leader loop start point ", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				err = s.watchLeader(leader.kv.ModRevision)
				if err != nil {
					log.Info("try to watch leader with the latest revision")
					err = s.watchLeader(0)
				}
				log.Info("leader changed, try to campaign leader")
				continue
			}
		}
		// now the `leader` is zero value, indicating that there is no leader,
		// therefore a leader campaign is followed
		// assume binlog has already catch up, no need to wait. so comment waitUntilBinlogCatchup
		// <-s.waitUntilBinlogCatchup()
		if s.isOnlyFollow() {
			log.Info("current node is an only follow node, do not participaate in leader campaign")
			time.Sleep(1 * time.Second)
			continue
		}

		// if leader.Term == 0, then it indicates cluster is new, so no need to preCampaign
		// TODO need to compare leader.Term and s.term or not ?
		// TODO !!! move leader.term check into preCampaign, or the init mode of cluster might has single point mode
		var isSinglePoint bool
		log.Info("current node isPreviousMaster? ", s.isPreviousMaster(leader))
		if leader.meta.term != 0 {
			log.Info("begin preCampaign")
			isSinglePoint = s.preCampaign(s.isPreviousMaster(leader))
			log.Info("preCampaign done, current node is single point? ", isSinglePoint)
		}
		// add isSinglePoint Logic
		if isSinglePoint {
			s.becomeSinglePointMaster(false)
			continue
		}

		campaignSuccess, lr, err := s.campaignLeader()
		if err != nil {
			// if leader campaign has error occurred,
			// one situation is that the leader is another node, with no error,
			// in which case, current node just get leader again and start a new watch
			//
			// one other situation may be that current node succeed in campaign
			// but fail in some proceeding process, timeout in return maybe.
			// In this case, current solution is try to delete current leader if the leader is current node.
			log.Errorf("campaign leader err, someone else may be the leader %s", errors.ErrorStack(err))
			s.deleteLeaderKey()
			continue
		}
		if !campaignSuccess {
			log.Info("campaign leader fail, someone else may be the leader")
			continue
		}
		s.updateLeaseExpireTS()
		s.term++
		keepLeaderAliveBarrier := make(chan interface{})
		// assert: s.leaderStopCh is nil or closed
		s.leaderStopCh = make(chan interface{})

		go func() {
			agentMasterSwitch.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName}).Inc()
			s.keepLeaderAliveLoop(int64(lr.ID))
			keepLeaderAliveBarrier <- "finish"
		}()
		// now current node is the leader
		err = s.promoteToMaster()
		if err != nil {
			log.Error("fail to set read write, err: ", err)
			s.setServiceReadonlyOrShutdown()
			close(s.leaderStopCh)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		<-keepLeaderAliveBarrier
	}

}

func (s *Server) setServiceReadonlyOrShutdown() {
	err := s.serviceManager.SetReadOnly()
	if err != nil {
		log.Warn("has error when set service readonly, so force close, err ", err)
		s.forceClose()
	}
}

// ChangeMaster triggers change master by endpoint
func (s *Server) ChangeMaster(w http.ResponseWriter, r *http.Request) {
	if r != nil {
		log.Info("receive ChangeMaster from ", r.RemoteAddr)
	}
	if s.amILeader() {
		// set service readonly to avoid brain-split
		s.setServiceReadonlyOrShutdown()
		// delete leader key
		s.deleteLeaderKey()
		// wait so that other agents are more likely to be the leader
		time.Sleep(500 * time.Millisecond)
		// inform leaderStopCh to stop keeping alive loop
		close(s.leaderStopCh)
		log.Info("current node is leader and has given up leader successfully")
		w.Write([]byte("current node is leader and has given up leader successfully\n"))
	} else {
		log.Info("current node is not leader, change master should be applied on leader")
		w.Write([]byte("current node is not leader, change master should be applied on leader\n"))
	}
}

// SetReadOnly sets mysql readonly
func (s *Server) SetReadOnly(w http.ResponseWriter, r *http.Request) {
	log.Info("receive setReadOnly request")
	if s.amILeader() {
		success, err := s.serviceManager.SetReadOnlyManually()
		if err != nil {
			log.Info("set current node readonly fail")
			log.Error("error when set current node readonly ", err)
			w.Write([]byte("set current node readonly fail\n"))
			w.Write([]byte("error when set current node readonly" + err.Error() + "\n"))
			return
		}
		if success {
			log.Info("set current node readonly success")
			w.Write([]byte(fmt.Sprint("set current node readonly success\n")))
		} else {
			log.Info("set current node readonly fail")
			w.Write([]byte(fmt.Sprint("set current node readonly fail\n")))
		}
		return
	}
	log.Info("set current node readonly fail")
	log.Info("current node is not leader, so no need to set readonly")
	w.Write([]byte("set current node readonly fail\n"))
	w.Write([]byte("current node is not leader, so no need to set readonly\n"))
}

// SetReadWrite sets mysql readwrite
func (s *Server) SetReadWrite(w http.ResponseWriter, r *http.Request) {
	log.Info("receive setReadWrite request")
	if s.amILeader() {
		err := s.serviceManager.SetReadWrite()
		if err != nil {
			log.Info("set current node readwrite fail")
			log.Error("error when set current node readwrite ", err)
			w.Write([]byte("set current node readwrite fail\n"))
			w.Write([]byte("error when set current node readwrite" + err.Error() + "\n"))
			return
		}
		log.Info("set current node readwrite success")
		w.Write([]byte("set current node readwrite success\n"))
		return
	}
	log.Info("set current node readwrite fail")
	log.Info("current node is not leader, so no need to set readwrite")
	w.Write([]byte("set current node readwrite fail\n"))
	w.Write([]byte("current node is not leader, so no need to set readwrite\n"))
}

// SetOnlyFollow sets the s.onlyFollow to true or false,
// depending on the param `onlyFollow` passed in
func (s *Server) SetOnlyFollow(w http.ResponseWriter, r *http.Request) {
	log.Info("receive setOnlyFollow request")
	operation, ok := r.URL.Query()["onlyFollow"]
	if !ok || len(operation) < 1 {
		log.Errorf("has no onlyFollow in query %+v ", r.URL.Query())
		w.Write([]byte(fmt.Sprintf("has no onlyFollow in query %+v ", r.URL.Query())))
		return
	}
	switch operation[0] {
	case "true":
		log.Info("set onlyFollow to true")
		s.onlyFollow = true
		w.Write([]byte("set onlyFollow to true"))
		break
	case "false":
		log.Info("set onlyFollow to false")
		s.onlyFollow = false
		w.Write([]byte("set onlyFollow to false"))
		break
	default:
		log.Info("set onlyFollow operation is undefined ", operation[0])
		w.Write([]byte("set onlyFollow operation is undefined " + operation[0]))
	}
}

func (s *Server) setIsLeaderToTrue() {
	atomic.StoreInt32(&s.isLeader, 1)
	agentIsLeader.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName}).Set(1)
}

func (s *Server) setIsLeaderToFalse() bool {
	agentIsLeader.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName}).Set(0)
	return atomic.CompareAndSwapInt32(&s.isLeader, 1, 0)
}

func (s *Server) amILeader() bool {
	return atomic.LoadInt32(&s.isLeader) == 1
}

func (s *Server) loadUUID() error {
	s.uuidRWLock.Lock()
	defer s.uuidRWLock.Unlock()
	uuid, err := mysql.GetServerUUID(s.db)
	if err != nil {
		return err
	}
	s.uuid = uuid
	latestPos.UUID = uuid
	return nil
}

func (s *Server) getUUID() string {
	s.uuidRWLock.RLock()
	defer s.uuidRWLock.RUnlock()
	return s.uuid
}

// isOnlyFollow returns whether current server is the node
// that only follows master, not participating in campaigning leader.
// if variable in memory, onlyFollow, is true, then server is onlyFollow,
// else it depends on s.cfg.OnlyFollow
func (s *Server) isOnlyFollow() bool {
	if s.onlyFollow {
		return true
	}
	return s.cfg.OnlyFollow
}

func (s *Server) isPreviousMaster(leader *Leader) bool {
	log.Infof("s.term: %d, s.node.ID(): %s, s.getUUID(): %s, leader: %+v ",
		s.term, s.node.ID(), s.getUUID(), leader)
	return (s.term == leader.meta.term && s.node.ID() == leader.meta.name) ||
		(s.term == leader.meta.term-1 && s.getUUID() == leader.meta.lastUUID)
}

// doChangeMaster does different things in the following different conditions:
// if a node is the former leader and current leader, do nothing
// if a node is the former leader but not current leader, downgrade
// if a node is not the former leader but current leader, promote to master
// if a node is not the former leader nor current leader, redirect master to new leader.
func (s *Server) doChangeMaster(leader *Leader) error {
	newLeaderID := leader.name

	log.Infof("the new leader id is %s", newLeaderID)
	log.Info("the new leader is current node? ", s.isSameLeader(newLeaderID))
	log.Info("current node is former leader? ", s.amILeader())

	if s.isSameLeader(newLeaderID) {
		// current node is the new leader
		return s.promoteToMaster()
	}
	// now current node is not the new leader
	newLeader, err := s.node.NodeStatus(s.ctx, newLeaderID)
	if err != nil {
		return errors.Trace(err)
	}
	log.Infof("new leader info is %+v", newLeader)

	masterHost, masterPort, err := parseHost(newLeader.InternalHost)
	if err != nil {
		return err
	}
	log.Infof("masterHost: %s, masterPort: %s", masterHost, masterPort)

	if s.setIsLeaderToFalse() {
		// current node is former leader and is not the new leader
		// downgrade to follower
		log.Info("current node is former master, downgrade to slave")
	}
	// current node is not leader any more, became slave and direct to new master
	return s.becomeSlave(masterHost, masterPort)
}

func (s *Server) promoteToMaster() error {
	// current node is the new leader
	if s.amILeader() {
		// current node is former leader
		log.Info("former leader is also new leader")
		return s.serviceManager.SetReadWrite()
	}
	// if current node is not the former leader, promote

	log.Info("current node is the new leader but not the former, begin to promote")
	s.setIsLeaderToTrue()
	// stop io/sql thread
	agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "sql_thread"}).Set(0)
	agentSlaveStatus.With(prometheus.Labels{"cluster_name": s.cfg.ClusterName, "type": "io_thread"}).Set(0)
	s.serviceManager.PromoteToMaster()
	// upload current mysql binlog info
	err := s.uploadPromotionBinlog()
	if err != nil {
		log.Error("has error when upload position during master promotion, "+
			"info may be inconsistent ", err)
	}
	return s.serviceManager.SetReadWrite()
}

// TODO move to service manager to decouple mysql and agent
func (s *Server) uploadPromotionBinlog() error {
	rSet, err := mysql.GetSlaveStatus(s.db)
	if err != nil {
		return errors.Trace(err)
	}
	if rSet["Relay_Master_Log_File"] == "" {
		log.Warn("do not have Relay_Master_Log_File, this node is not a former slave, "+
			"it may be the initialization of this cluster, slave status is ", rSet)
		return nil
	}
	var currentPos Position
	currentPos.File = rSet["Relay_Master_Log_File"]
	currentPos.Pos = rSet["Exec_Master_Log_Pos"]
	currentPos.GTID = rSet["Executed_Gtid_Set"]
	currentPos.UUID = latestPos.UUID
	// key := join("switch", fmt.Sprint(time.Now().Unix()), s.node.ID())
	key := join("switch", fmt.Sprint(s.term-1), s.node.ID())
	latestPosJSONBytes, err := json.Marshal(currentPos)
	if err != nil {
		return errors.Trace(err)
	}
	err = s.node.RawClient().Put(s.ctx, key, string(latestPosJSONBytes))
	return errors.Trace(err)
}

func (s *Server) becomeSlave(masterHost, masterPort string) error {
	// After new master has been promoted
	err := s.serviceManager.RedirectMaster(masterHost, masterPort)
	retry := 0
	for err != nil && retry < 10 {
		log.Error("redirect master has error, self-spin ", err)
		if err != driver.ErrBadConn {
			time.Sleep(100 * time.Millisecond)
		}
		err = s.serviceManager.RedirectMaster(masterHost, masterPort)
		retry++
	}

	return err
}

// preCampaign prepares campaign information and does the comparison
// 1. upload (term + 1), last UUID and last GTID as election log
// 2. get all election logs from etcd
// 3. compare all the logs with itself, determine whether it is the latest? whether it is the single point?
// 4. returns whether it is the single point
func (s *Server) preCampaign(isFormerMaster bool) (isSinglePoint bool) {
	// wait for consuming relay log
	log.Infof("enter preCampaign as the former master? %v", isFormerMaster)
	time.Sleep(s.cfg.CampaignWaitTime)
	if isFormerMaster {
		s.uploadLogForElectionAsFormerMaster()
	} else {
		s.uploadLogForElectionAsSlave()
	}
	log.Infof("logForElection has been uploaded")
	// wait for other node uploading
	time.Sleep(s.cfg.CampaignWaitTime)
	logs, err := s.getAllLogsForElection()
	for err != nil {
		log.Error("error when getAllLogsForElection ", err)
		time.Sleep(100 * time.Millisecond)
		logs, err = s.getAllLogsForElection()
	}
	log.Infof("get all logsForElection from etcd")
	isLatest, isSinglePoint := s.isLatestLog(logs)
	log.Infof("current node is the latest? %v. all logs are %+v", isLatest, logs)

	if !isLatest {
		time.Sleep(s.cfg.CampaignWaitTime)
	}
	if isFormerMaster {
		log.Info("current node is the former master, wait for some more time")
		time.Sleep(500 * time.Millisecond)
	}
	return isSinglePoint
}

// preWatch validates the gtid.
// current agent gtid is compared with the gtid uploaded by new master.
// if current agent has later gtid, then preWatch returns error
// else agent's term is updated and persisted
func (s *Server) preWatch(leader *Leader) (err error) {
	defer func() {
		if err == nil {
			s.persistMyTerm()
		}
	}()
	if s.term == 0 {
		// a fast return: if current node is the new joiner, it is assumed that the data is no later than the master.
		log.Info("server's term is 0 so server is just started, assume no later log")
		s.term = leader.meta.term
		return nil
	}
	if s.term < leader.meta.term-1 {
		// a fast return: if current node is behind master for more than 2 term,
		// it is "incomparable" with the master node, so fail fast
		log.Errorf("current server %s is behind current term for more than one term", s.node.ID())
		return errors.NotValidf("current server %s is behind current term for more than one term", s.node.ID())
	}
	if s.term == leader.meta.term {
		// a fast return: if current node has the same term with the master, it means that all validations have been passed
		// and this may by happen when current agent is restarting
		log.Warnf("current node has term %d while leader has term %d. so current node could be slave", s.term, leader.meta.term)
		return nil
	}

	log.Info("current node isPreviousMaster? ", s.isPreviousMaster(leader))
	if s.lastUUID == "" {
		// if current node is the previous master, s.lastUUID is itself
		if s.isPreviousMaster(leader) {
			err = s.loadMasterLogFromMySQL()
		} else {
			// maybe it is a restart agent, load from mysql
			err = s.loadSlaveLogFromMySQL()
		}
		if err != nil {
			log.Errorf("has error when loadLogFromMySQL: %+v ", err)
			return errors.Trace(err)
		}
	}
	log.Infof("s.lastUUID is %s and leader is %+v", s.lastUUID, leader)

	if s.term == leader.meta.term-1 {
		s.term = leader.meta.term
	} else {
		// this situation is s.term is greater than leader.meta.term, which should not happen, so log it
		log.Errorf("current node has term %d while leader has term %d", s.term, leader.meta.term)
	}
	if s.lastUUID != leader.meta.lastUUID {
		log.Errorf("current server %s has different leader UUID: %s vs %s ",
			s.node.ID(), s.lastUUID, leader.meta.lastUUID)
		return errors.NotValidf("current server %s has different leader UUID: %s vs %s ",
			s.node.ID(), s.lastUUID, leader.meta.lastUUID)
	}
	myTxnID, err := mysql.GetTxnIDFromGTIDStr(s.lastGTID, s.lastUUID)
	if err != nil {
		log.Errorf("error when extract myTxnID")
		return errors.Trace(err)
	}
	latestTxnID, err := mysql.GetTxnIDFromGTIDStr(leader.meta.lastGTID, leader.meta.lastUUID)
	if err != nil {
		log.Errorf("error when extract leader's latestTxnID")
		return errors.Trace(err)
	}
	if latestTxnID < myTxnID {
		log.Errorf("current server %s has later GTID: %s vs %s . leader: %+v ",
			s.node.ID(), s.lastGTID, leader.meta.lastGTID, leader)
		return errors.NotValidf("current server %s has later GTID: %s vs %s . leader: %+v ",
			s.node.ID(), s.lastGTID, leader.meta.lastGTID, leader)
	}
	// TODO add logic: if current gtid is ahead master gtid, then deregister current node
	return nil
}