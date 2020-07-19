package acfundanmu

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"sort"

	"github.com/orzogc/acfundanmu/acproto"

	"github.com/Workiva/go-datastructures/queue"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// 处理接受到的数据里的命令
func (t *token) handleCommand(ctx context.Context, c *websocket.Conn, stream *acproto.DownstreamPayload, q *queue.Queue, info *LiveInfo, hb chan<- int64) (e error) {
	defer func() {
		if err := recover(); err != nil {
			e = fmt.Errorf("handleCommand() error: %w", err)
		}
	}()

	if stream == nil {
		return
	}

	switch stream.Command {
	case "Global.ZtLiveInteractive.CsCmd":
		cmd := &acproto.ZtLiveCsCmdAck{}
		err := proto.Unmarshal(stream.PayloadData, cmd)
		checkErr(err)
		switch cmd.CmdAckType {
		case "ZtLiveCsEnterRoomAck":
			enterRoom := &acproto.ZtLiveCsEnterRoomAck{}
			err = proto.Unmarshal(cmd.Payload, enterRoom)
			checkErr(err)
			if enterRoom.HeartbeatIntervalMs > 0 {
				hb <- enterRoom.HeartbeatIntervalMs
			} else {
				hb <- 10000
			}
		case "ZtLiveCsHeartbeatAck":
			heartbeat := &acproto.ZtLiveCsHeartbeatAck{}
			err = proto.Unmarshal(cmd.Payload, heartbeat)
			checkErr(err)
		case "ZtLiveCsUserExitAck":
			//userExit := &acproto.ZtLiveCsUserExitAck{}
			//err = proto.Unmarshal(cmd.Payload, userExit)
			//checkErr(err)
		default:
			log.Printf("未知的cmd.CmdAckType：%s\npayload string:\n%s\npayload base64:\n%s\n",
				cmd.CmdAckType,
				string(cmd.Payload),
				base64.StdEncoding.EncodeToString(cmd.Payload))
		}
	case "Basic.KeepAlive":
		keepalive := &acproto.KeepAliveResponse{}
		err := proto.Unmarshal(stream.PayloadData, keepalive)
		checkErr(err)
	case "Basic.Ping":
		ping := &acproto.PingResponse{}
		err := proto.Unmarshal(stream.PayloadData, ping)
		checkErr(err)
	case "Basic.Unregister":
		unregister := &acproto.UnregisterResponse{}
		err := proto.Unmarshal(stream.PayloadData, unregister)
		checkErr(err)
		c.Close(websocket.StatusNormalClosure, "Unregister")
	case "Push.ZtLiveInteractive.Message":
		err := c.Write(ctx, websocket.MessageBinary, *t.pushMessage())
		checkErr(err)
		message := &acproto.ZtLiveScMessage{}
		err = proto.Unmarshal(stream.PayloadData, message)
		checkErr(err)
		payload := message.Payload
		if message.CompressionType == acproto.ZtLiveScMessage_GZIP {
			r, err := gzip.NewReader(bytes.NewReader(message.Payload))
			checkErr(err)
			result, err := ioutil.ReadAll(r)
			checkErr(err)
			payload = result
		}
		switch message.MessageType {
		case "ZtLiveScActionSignal":
			handleMsgAct(&payload, q, info, t.gifts)
		case "ZtLiveScStateSignal":
			handleMsgState(&payload, info)
		case "ZtLiveScStatusChanged":
			statusChanged := &acproto.ZtLiveScStatusChanged{}
			err := proto.Unmarshal(payload, statusChanged)
			checkErr(err)
			if statusChanged.Type == acproto.ZtLiveScStatusChanged_LIVE_CLOSED || statusChanged.Type == acproto.ZtLiveScStatusChanged_LIVE_BANNED {
				t.wsStop(ctx, c, "直播已经结束")
			}
		case "ZtLiveScTicketInvalid":
			ticketInvalid := &acproto.ZtLiveScTicketInvalid{}
			err := proto.Unmarshal(payload, ticketInvalid)
			checkErr(err)
			t.ticketIndex = (t.ticketIndex + 1) % len(t.tickets)
			err = c.Write(ctx, websocket.MessageBinary, *t.enterRoom())
			checkErr(err)
		default:
			log.Printf("未知的message.MessageType：%s\npayload string:\n%s\npayload base64:\n%s\n",
				message.MessageType,
				string(payload),
				base64.StdEncoding.EncodeToString(payload))
		}
	case "Push.Message":
		msg := &acproto.Message_Message{}
		err := proto.Unmarshal(stream.PayloadData, msg)
		checkErr(err)
		switch msg.ContentType {
		case int32(acproto.Cloud_Message_TEXT):
			txt := &acproto.Cloud_Message_Text{}
			err = proto.Unmarshal(msg.Content, txt)
		default:
			log.Println("未知的IM Push.Message：", msg.ContentType)
		}
	default:
		if stream.ErrorCode > 0 {
			log.Println("Error: ", stream.ErrorCode, stream.ErrorMsg)
			if stream.ErrorCode == 10018 {
				t.wsStop(ctx, c, "Log out")
			}
			log.Println(string(stream.ErrorData))
		} else {
			log.Printf("未知的stream.Command：%s\npayload string:\n%s\npayload base64:\n%s\n",
				stream.Command,
				string(stream.PayloadData),
				base64.StdEncoding.EncodeToString(stream.PayloadData))
		}
	}

	return nil
}

// 处理action signal数据
func handleMsgAct(payload *[]byte, q *queue.Queue, info *LiveInfo, gifts map[int]Giftdetail) {
	actionSignal := &acproto.ZtLiveScActionSignal{}
	err := proto.Unmarshal(*payload, actionSignal)
	checkErr(err)

	var danmu []DanmuMessage
	for _, item := range actionSignal.Item {
		for _, pl := range item.Payload {
			switch item.SingalType {
			case "CommonActionSignalComment":
				comment := &acproto.CommonActionSignalComment{}
				err = proto.Unmarshal(pl, comment)
				checkErr(err)
				d := DanmuMessage{
					Type:     Comment,
					SendTime: comment.SendTimeMs * 1e6,
					UserID:   comment.UserInfo.UserId,
					Nickname: comment.UserInfo.Nickname,
					//Avatar:   comment.UserInfo.Avatar[0].Url,
					Comment: comment.Content}
				danmu = append(danmu, d)
			case "CommonActionSignalLike":
				like := &acproto.CommonActionSignalLike{}
				err = proto.Unmarshal(pl, like)
				checkErr(err)
				d := DanmuMessage{
					Type:     Like,
					SendTime: like.SendTimeMs * 1e6,
					UserID:   like.UserInfo.UserId,
					Nickname: like.UserInfo.Nickname,
					//Avatar:   like.UserInfo.Avatar[0].Url,
				}
				danmu = append(danmu, d)
			case "CommonActionSignalUserEnterRoom":
				enter := &acproto.CommonActionSignalUserEnterRoom{}
				err = proto.Unmarshal(pl, enter)
				checkErr(err)
				d := DanmuMessage{
					Type:     EnterRoom,
					SendTime: enter.SendTimeMs * 1e6,
					UserID:   enter.UserInfo.UserId,
					Nickname: enter.UserInfo.Nickname,
					//Avatar:   enter.UserInfo.Avatar[0].Url,
				}
				danmu = append(danmu, d)
			case "CommonActionSignalUserFollowAuthor":
				follow := &acproto.CommonActionSignalUserFollowAuthor{}
				err = proto.Unmarshal(pl, follow)
				checkErr(err)
				d := DanmuMessage{
					Type:     FollowAuthor,
					SendTime: follow.SendTimeMs * 1e6,
					UserID:   follow.UserInfo.UserId,
					Nickname: follow.UserInfo.Nickname,
					//Avatar:   follow.UserInfo.Avatar[0].Url,
				}
				danmu = append(danmu, d)
			case "CommonNotifySignalKickedOut":
				kickedOut := &acproto.CommonNotifySignalKickedOut{}
				err = proto.Unmarshal(pl, kickedOut)
				checkErr(err)
				infoMutex.Lock()
				info.KickedOut = kickedOut.Reason
				infoMutex.Unlock()
			case "CommonNotifySignalViolationAlert":
				violationAlert := &acproto.CommonNotifySignalViolationAlert{}
				err = proto.Unmarshal(pl, violationAlert)
				checkErr(err)
				infoMutex.Lock()
				info.ViolationAlert = violationAlert.ViolationContent
				infoMutex.Unlock()
			case "AcfunActionSignalThrowBanana":
				banana := &acproto.AcfunActionSignalThrowBanana{}
				err = proto.Unmarshal(pl, banana)
				checkErr(err)
				d := DanmuMessage{
					Type:        ThrowBanana,
					SendTime:    banana.SendTimeMs * 1e6,
					UserID:      banana.Visitor.UserId,
					Nickname:    banana.Visitor.Name,
					BananaCount: int(banana.Count),
				}
				danmu = append(danmu, d)
			case "CommonActionSignalGift":
				gift := &acproto.CommonActionSignalGift{}
				err = proto.Unmarshal(pl, gift)
				checkErr(err)
				d := DanmuMessage{
					Type:     Gift,
					SendTime: gift.SendTimeMs * 1e6,
					UserID:   gift.User.UserId,
					Nickname: gift.User.Nickname,
					Avatar:   gift.User.Avatar[0].Url,
					Gift: GiftInfo{
						Giftdetail:            gifts[int(gift.GiftId)],
						Count:                 int(gift.Count),
						Combo:                 int(gift.Combo),
						Value:                 int(gift.Value),
						ComboID:               gift.ComboId,
						SlotDisplayDurationMs: int(gift.SlotDisplayDurationMs),
						ExpireDurationMs:      int(gift.ExpireDurationMs),
					},
				}
				danmu = append(danmu, d)
			default:
				log.Printf("未知的Action Signal item.SingalType：%s\npayload string:\n%s\npayload base64:\n%s\n",
					item.SingalType,
					string(pl),
					base64.StdEncoding.EncodeToString(pl))
			}
		}
	}

	// 按SendTime大小排序
	sort.Slice(danmu, func(i, j int) bool {
		return danmu[i].SendTime < danmu[j].SendTime
	})

	for _, d := range danmu {
		err = q.Put(d)
		checkErr(err)
	}
}

// 处理state signal数据
func handleMsgState(payload *[]byte, info *LiveInfo) {
	signal := &acproto.ZtLiveScStateSignal{}
	err := proto.Unmarshal(*payload, signal)
	checkErr(err)

	for _, item := range signal.Item {
		switch item.SingalType {
		case "AcfunStateSignalDisplayInfo":
			bananaInfo := &acproto.AcfunStateSignalDisplayInfo{}
			err = proto.Unmarshal(item.Payload, bananaInfo)
			checkErr(err)
			infoMutex.Lock()
			info.AllBananaCount = bananaInfo.BananaCount
			infoMutex.Unlock()
		case "CommonStateSignalDisplayInfo":
			stateInfo := &acproto.CommonStateSignalDisplayInfo{}
			err = proto.Unmarshal(item.Payload, stateInfo)
			checkErr(err)
			infoMutex.Lock()
			info.WatchingCount = stateInfo.WatchingCount
			info.LikeCount = stateInfo.LikeCount
			info.LikeDelta = int(stateInfo.LikeDelta)
			infoMutex.Unlock()
		case "CommonStateSignalTopUsers":
			topUsers := &acproto.CommonStateSignalTopUsers{}
			err = proto.Unmarshal(item.Payload, topUsers)
			checkErr(err)
			var users []TopUser
			for _, user := range topUsers.User {
				u := TopUser{
					UserID:                 user.UserInfo.UserId,
					Nickname:               user.UserInfo.Nickname,
					Avatar:                 user.UserInfo.Avatar[0].Url,
					CustomWatchingListData: user.CustomWatchingListData,
					DisplaySendAmount:      user.DisplaySendAmount,
					AnonymousUser:          user.AnonymousUser,
				}
				users = append(users, u)
			}
			infoMutex.Lock()
			info.TopUsers = users
			infoMutex.Unlock()
		case "CommonStateSignalRecentComment":
			comments := &acproto.CommonStateSignalRecentComment{}
			err = proto.Unmarshal(item.Payload, comments)
			checkErr(err)
			var danmu []DanmuMessage
			for _, comment := range comments.Comment {
				d := DanmuMessage{
					Type:     Comment,
					SendTime: comment.SendTimeMs * 1e6,
					UserID:   comment.UserInfo.UserId,
					Nickname: comment.UserInfo.Nickname,
					//Avatar:   comment.UserInfo.Avatar[0].Url,
					Comment: comment.Content,
				}
				danmu = append(danmu, d)
			}
			// 按SendTime大小排序
			//sort.Slice(danmu, func(i, j int) bool {
			//	return danmu[i].SendTime < danmu[j].SendTime
			//})
			infoMutex.Lock()
			info.RecentComment = danmu
			infoMutex.Unlock()
		case "CommonStateSignalChatCall":
			//chatCall := &acproto.CommonStateSignalChatCall{}
			//err = proto.Unmarshal(item.Payload, chatCall)
			//checkErr(err)
		case "CommonStateSignalChatAccept":
			//chatAccept := &acproto.CommonStateSignalChatAccept{}
			//err = proto.Unmarshal(item.Payload, chatAccept)
			//checkErr(err)
		case "CommonStateSignalChatReady":
			//chatReady := &acproto.CommonStateSignalChatReady{}
			//err = proto.Unmarshal(item.Payload, chatReady)
			//checkErr(err)
		case "CommonStateSignalChatEnd":
			//chatEnd := &acproto.CommonStateSignalChatEnd{}
			//err = proto.Unmarshal(item.Payload, chatEnd)
			//checkErr(err)
		default:
			log.Printf("未知的State Signal item.SingalType：%s\npayload string:\n%s\npayload base64:\n%s\n",
				item.SingalType,
				string(item.Payload),
				base64.StdEncoding.EncodeToString(item.Payload))
		}
	}
}
