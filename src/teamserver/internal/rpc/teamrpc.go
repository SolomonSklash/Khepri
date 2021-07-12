/*
 * Copyright (c) 2021.  https://github.com/geemion
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rpc

import (
	"context"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
	"log"
	"net"
	"sync"
	"teamserver/internal/client"
	"teamserver/internal/conf"
	"teamserver/internal/handler"
	pb "teamserver/internal/proto/protobuf"
	"teamserver/internal/server"
	"teamserver/internal/store"
	"teamserver/pkg/mq"
)

type TeamRPCService struct {
	pb.UnimplementedTeamRPCServiceServer
	cmdqueue         *mq.Client
	rpcclient        sync.Map
	beaconMsgHandler *handler.MsgHandler
	serverMgr        *server.ServerMgr
}

func (t *TeamRPCService) Login(ctx context.Context, req *pb.LoginUserReq) (rsp *pb.LoginUserRsp, err error) {

	if ctx.Err() == context.Canceled {
		return nil, errors.New("cannel")
	}

	rsp = &pb.LoginUserRsp{}

	if len(req.Username) == 0 || len(req.Passwdhash) == 0 {
		err = errors.New("invalid username or password")
		rsp.Error = err.Error()
		return
	}

	if conf.GlobalConf.TeamclientSecret != req.Passwdhash {
		err = errors.New("error password")
		rsp.Error = err.Error()
		return
	}

	uuid, err := uuid.NewUUID()
	if err != nil {
		return
	}

	token := uuid.String()
	peerctx, _ := peer.FromContext(ctx)
	ip := peerctx.Addr.String()

	client := &client.TeamClient{}
	client.Username = req.Username
	client.ClientAddr = ip
	client.Token = token

	t.rpcclient.Store(token, client)
	rsp.Token = token

	return rsp, nil
}

//todo: send error to teamclient
func (t *TeamRPCService) CommandChannel(channel pb.TeamRPCService_CommandChannelServer) (err error) {
	wg := sync.WaitGroup{}
	wg.Add(2)

	rspch, err := t.cmdqueue.Subscribe(conf.CmdRspTopic)
	if err != nil {
		return err
	}

	go func() {
		for {
			data, err := channel.Recv()
			if err != nil {
				log.Println(err)
				t.cmdqueue.Publish(conf.CmdRspTopic, "exit")
				break
			}
			t.cmdqueue.Publish(conf.CmdReqTopic, data)
			store.AddTask(data.MsgId, data.BeaconId, data.ByteValue)
			log.Println(data.BeaconId)
		}
		wg.Done()
	}()

	go func() {
		defer t.cmdqueue.Unsubscribe(conf.CmdRspTopic, rspch)
		for {
			data, ok := (t.cmdqueue.GetPayLoad(rspch)).(pb.CommandRsp)
			if !ok {
				log.Println("error rsp")
				break
			}
			store.UpdateTask(data.TaskId, data.ByteValue)
			channel.Send(&data)
		}
		wg.Done()
	}()

	wg.Wait()
	fmt.Println("CommandChannel exit")
	return nil
}

func (t *TeamRPCService) ServerCmd(ctx context.Context, req *pb.ServerCmdReq) (rsp *pb.ServerCmdRsp, err error) {
	if ctx.Err() == context.Canceled {
		return nil, errors.New("cannel")
	}

	var data []byte
	rspCmdid := req.CmdId
	switch pb.CMDID(req.CmdId) {
	case pb.CMDID_GET_BEACONS_REQ:
		{
			data, err = store.GetBeacons()
			break
		}
	case pb.CMDID_START_BEACON_SERVER:
		{
			item := &pb.ServerItem{}
			err := proto.Unmarshal(req.ByteValue, item)
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
			err = t.serverMgr.StartServer(item.Name, item.Addr, t.beaconMsgHandler)
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
			rspCmdid = int32(pb.CMDID_GET_BEACON_SERVER)
			data, err = t.serverMgr.GetRunningServer()
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
			break
		}
	case pb.CMDID_STOP_BEACON_SERVER:
		{
			item := &pb.ServerItem{}
			err := proto.Unmarshal(req.ByteValue, item)
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
			err = t.serverMgr.StopServer(item.Name)
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
			rspCmdid = int32(pb.CMDID_GET_BEACON_SERVER)
			data, err = t.serverMgr.GetRunningServer()
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
		}
	case pb.CMDID_GET_BEACON_SERVER:
		{
			data, err = t.serverMgr.GetRunningServer()
			if err != nil {
				data = t.setErrorMsg(rspCmdid, err)
				rspCmdid = int32(pb.CMDID_ERROR_MSG)
				break
			}
			break
		}
	default:
		break
	}

	return &pb.ServerCmdRsp{CmdId: rspCmdid, ByteValue: data}, nil
}

func (t *TeamRPCService) setErrorMsg(cmdid int32, err error) (data []byte) {
	errorMsg := &pb.ErrorMsg{}
	errorMsg.Cmdid = cmdid
	errorMsg.Error = err.Error()
	data, _ = proto.Marshal(errorMsg)
	return data
}

func NewTeamRpcService(addr string, mqclient *mq.Client, maxSendSize int, maxRecvSize int) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("net.Listen err: %v", err)
	}

	options := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxRecvSize),
		grpc.MaxSendMsgSize(maxSendSize),
	}

	serverMgr := &server.ServerMgr{}
	msgHandler := handler.NewMsgHandler(mqclient)

	rpcServer := grpc.NewServer(options...)
	pb.RegisterTeamRPCServiceServer(rpcServer, &TeamRPCService{cmdqueue: mqclient, beaconMsgHandler: msgHandler, serverMgr: serverMgr})
	return rpcServer.Serve(lis)
}