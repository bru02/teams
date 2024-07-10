// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"maunium.net/go/mautrix/bridgev2/database"

	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-slack/pkg/slackid"
)

func (s *SlackClient) HandleSlackEvent(rawEvt any) {
	log := s.UserLogin.Log.With().
		Str("action", "handle slack event").
		Type("event_type", rawEvt).
		Logger()
	ctx := log.WithContext(context.TODO())
	switch evt := rawEvt.(type) {
	case *slack.ConnectingEvent:
		log.Debug().
			Int("attempt_num", evt.Attempt).
			Int("connection_count", evt.ConnectionCount).
			Msg("Connecting to Slack")
		s.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	case *slack.ConnectedEvent:
		if evt.Info.Team.ID != s.TeamID || evt.Info.User.ID != s.UserID {
			log.Error().
				Str("event_team_id", evt.Info.Team.ID).
				Str("event_user_id", evt.Info.User.ID).
				Msg("User login ID mismatch in Connected event")
			s.invalidateSession(ctx, status.BridgeState{
				StateEvent: status.StateUnknownError,
				Error:      "slack-id-mismatch",
			})
			return
		}
		s.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
	case *slack.HelloEvent:
		// Ignored for now
	case *slack.InvalidAuthEvent:
		s.invalidateSession(ctx, status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "slack-invalid-auth",
		})
	case *slack.RTMError:
		log.Err(evt).Msg("Got RTM error")
		s.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      status.BridgeStateErrorCode(fmt.Sprintf("slack-rtm-error-%d", evt.Code)),
			Message:    fmt.Sprintf("%d: %s", evt.Code, evt.Msg),
		})
	case *slack.MessageEvent, *slack.ReactionAddedEvent, *slack.ReactionRemovedEvent,
		*slack.UserTypingEvent, *slack.ChannelMarkedEvent, *slack.IMMarkedEvent, *slack.GroupMarkedEvent,
		*slack.ChannelJoinedEvent, *slack.ChannelLeftEvent, *slack.GroupJoinedEvent, *slack.GroupLeftEvent,
		*slack.MemberJoinedChannelEvent, *slack.MemberLeftChannelEvent,
		*slack.ChannelUpdateEvent:
		wrapped, err := s.wrapEvent(ctx, evt)
		if err != nil {
			log.Err(err).Msg("Failed to wrap Slack event")
		} else {
			s.UserLogin.Bridge.QueueRemoteEvent(s.UserLogin, wrapped)
		}
	case *slack.EmojiChangedEvent:
		//go ut.handleEmojiChange(ctx, evt)
	case *slack.FileSharedEvent, *slack.FilePublicEvent, *slack.FilePrivateEvent,
		*slack.FileCreatedEvent, *slack.FileChangeEvent, *slack.FileDeletedEvent,
		*slack.DesktopNotificationEvent, *slack.ReconnectUrlEvent, *slack.LatencyReport:
		// ignored intentionally, these are duplicates or do not contain useful information
	default:
		logEvt := log.Warn()
		if log.GetLevel() == zerolog.TraceLevel {
			logEvt = logEvt.Any("event_data", evt)
		}
		logEvt.Msg("Unrecognized Slack event type")
	}
}

func (s *SlackClient) wrapEvent(ctx context.Context, rawEvt any) (bridgev2.RemoteEvent, error) {
	var meta SlackEventMeta
	var metaErr error
	var wrapped bridgev2.RemoteEvent
	meta.LogContext = func(c zerolog.Context) zerolog.Context { return c }
	switch evt := rawEvt.(type) {
	case *slack.MessageEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, evt.User, evt.Timestamp)
		meta.Type = bridgev2.RemoteEventMessage
		meta.CreatePortal = true
		meta.LogContext = func(c zerolog.Context) zerolog.Context {
			return c.Str("message_ts", evt.Timestamp).Str("message_sender", evt.User)
		}
		wrapped = &SlackMessage{
			SlackEventMeta: &meta,
			Data:           evt,
			Client:         s,
		}

	case *slack.ReactionAddedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Item.Channel, nil, evt.User, evt.EventTimestamp)
		var err error
		wrapped, err = s.wrapReaction(ctx, &meta, evt.Reaction, true, evt.Item)
		if err != nil {
			return nil, fmt.Errorf("failed to get reaction info: %w", err)
		}
	case *slack.ReactionRemovedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Item.Channel, nil, evt.User, evt.EventTimestamp)
		wrapped, _ = s.wrapReaction(ctx, &meta, evt.Reaction, false, evt.Item)

	case *slack.UserTypingEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, evt.User, "")
		wrapped = wrapTyping(&meta)

	case *slack.ChannelMarkedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, s.UserID, evt.Timestamp)
		wrapped = wrapReadReceipt(&meta)
	case *slack.IMMarkedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, s.UserID, evt.Timestamp)
		wrapped = wrapReadReceipt(&meta)
	case *slack.GroupMarkedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, s.UserID, evt.Timestamp)
		wrapped = wrapReadReceipt(&meta)

	case *slack.ChannelJoinedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel.ID, &evt.Channel, s.UserID, "")
		meta.CreatePortal = true
		wrapped = wrapMemberChange(&meta, meta.Sender, event.MembershipJoin, "")
	case *slack.ChannelLeftEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, s.UserID, evt.Timestamp)
		wrapped = wrapMemberChange(&meta, meta.Sender, event.MembershipLeave, event.MembershipJoin)
	case *slack.GroupJoinedEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel.ID, &evt.Channel, s.UserID, "")
		meta.CreatePortal = true
		wrapped = wrapMemberChange(&meta, meta.Sender, event.MembershipJoin, "")
	case *slack.GroupLeftEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, s.UserID, evt.Timestamp)
		wrapped = wrapMemberChange(&meta, meta.Sender, event.MembershipLeave, event.MembershipJoin)
	case *slack.MemberJoinedChannelEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, evt.User, "")
		wrapped = wrapMemberChange(&meta, meta.Sender, event.MembershipJoin, "")
	case *slack.MemberLeftChannelEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, evt.User, "")
		wrapped = wrapMemberChange(&meta, meta.Sender, event.MembershipLeave, event.MembershipJoin)

	case *slack.ChannelUpdateEvent:
		meta, metaErr = s.makeEventMeta(ctx, evt.Channel, nil, "", evt.Timestamp)
		meta.Type = bridgev2.RemoteEventChatInfoChange
		meta.CreatePortal = true
		fullChatInfo, err := s.fetchChatInfo(ctx, evt.Channel, false)
		if err != nil {
			return nil, fmt.Errorf("failed to get chat info: %w", err)
		}
		wrapped = &SlackChatInfoChange{
			SlackEventMeta: &meta,
			Change:         &bridgev2.ChatInfoChange{ChatInfo: fullChatInfo},
		}
	}
	return wrapped, metaErr
}

func (s *SlackClient) wrapReaction(ctx context.Context, meta *SlackEventMeta, reaction string, add bool, target slack.ReactionItem) (*SlackReaction, error) {
	if add {
		meta.Type = bridgev2.RemoteEventReaction
	} else {
		meta.Type = bridgev2.RemoteEventReactionRemove
	}
	shortcode := fmt.Sprintf(":%s:", reaction)
	slackReactionInfo := map[string]any{
		"name": reaction,
	}
	emoji, isImage := s.GetEmoji(ctx, reaction)
	if isImage {
		slackReactionInfo["mxc"] = emoji
		if !s.Main.Config.CustomEmojiReactions {
			emoji = shortcode
		}
	}
	extraContent := map[string]any{
		"com.beeper.reaction.shortcode": shortcode,
		"fi.mau.slack.reaction":         slackReactionInfo,
	}
	return &SlackReaction{
		SlackEventMeta: meta,
		Emoji:          emoji,
		EmojiID:        networkid.EmojiID(reaction),
		Meta:           extraContent,
		TargetID:       slackid.MakeMessageID(s.TeamID, target.Channel, target.Timestamp),
	}, nil
}

func wrapTyping(meta *SlackEventMeta) *SlackTyping {
	meta.Type = bridgev2.RemoteEventTyping
	return &SlackTyping{SlackEventMeta: meta}
}

func wrapReadReceipt(meta *SlackEventMeta) *SlackReadReceipt {
	meta.Type = bridgev2.RemoteEventReadReceipt
	return &SlackReadReceipt{SlackEventMeta: meta}
}

func wrapMemberChange(meta *SlackEventMeta, sender bridgev2.EventSender, newMembership, prevMembership event.Membership) *SlackChatInfoChange {
	meta.Type = bridgev2.RemoteEventChatInfoChange
	return &SlackChatInfoChange{
		SlackEventMeta: meta,
		Change: &bridgev2.ChatInfoChange{
			MemberChanges: &bridgev2.ChatMemberList{
				Members: []bridgev2.ChatMember{{
					EventSender:    sender,
					Membership:     newMembership,
					PrevMembership: prevMembership,
				}},
			},
		},
	}
}

func (s *SlackClient) makeEventMeta(ctx context.Context, channelID string, channel *slack.Channel, senderID, timestamp string) (meta SlackEventMeta, err error) {
	if channel != nil {
		meta.PortalKey = s.makePortalKey(channel)
	} else {
		meta.PortalKey, err = s.UserLogin.Bridge.FindPortalReceiver(ctx, slackid.MakePortalID(s.TeamID, channelID), s.UserLogin.ID)
		if err != nil {
			err = fmt.Errorf("failed to find portal receiver: %w", err)
			return
		} else if meta.PortalKey.IsEmpty() {
			// TODO fetch channel meta from server?
			err = fmt.Errorf("unknown channel %s", channelID)
			return
		}
	}
	if senderID != "" {
		meta.Sender = s.makeEventSender(senderID)
	}
	if timestamp != "" {
		meta.ID = slackid.MakeMessageID(s.TeamID, channelID, timestamp)
		meta.Timestamp = slackid.ParseSlackTimestamp(timestamp)
	}
	return
}

type SlackEventMeta struct {
	Type         bridgev2.RemoteEventType
	PortalKey    networkid.PortalKey
	Sender       bridgev2.EventSender
	Timestamp    time.Time
	ID           networkid.MessageID
	LogContext   func(zerolog.Context) zerolog.Context
	CreatePortal bool
}

func (s *SlackEventMeta) GetType() bridgev2.RemoteEventType {
	return s.Type
}

func (s *SlackEventMeta) GetPortalKey() networkid.PortalKey {
	return s.PortalKey
}

func (s *SlackEventMeta) AddLogContext(c zerolog.Context) zerolog.Context {
	return s.LogContext(c)
}

func (s *SlackEventMeta) GetSender() bridgev2.EventSender {
	return s.Sender
}

func (s *SlackEventMeta) ShouldCreatePortal() bool {
	return s.CreatePortal
}

func (s *SlackEventMeta) GetTimestamp() time.Time {
	if s.Timestamp.IsZero() {
		return time.Now()
	}
	return s.Timestamp
}

func (s *SlackEventMeta) GetID() networkid.MessageID {
	return s.ID
}

var (
	_ bridgev2.RemoteEvent                    = (*SlackEventMeta)(nil)
	_ bridgev2.RemoteEventWithTimestamp       = (*SlackEventMeta)(nil)
	_ bridgev2.RemoteEventThatMayCreatePortal = (*SlackEventMeta)(nil)
)

type SlackChatInfoChange struct {
	*SlackEventMeta
	Change *bridgev2.ChatInfoChange
}

func (s *SlackChatInfoChange) GetChatInfoChange(ctx context.Context) (*bridgev2.ChatInfoChange, error) {
	return s.Change, nil
}

var _ bridgev2.RemoteChatInfoChange = (*SlackChatInfoChange)(nil)

type SlackReadReceipt struct {
	*SlackEventMeta
}

var _ bridgev2.RemoteReceipt = (*SlackReadReceipt)(nil)

func (s *SlackReadReceipt) GetLastReceiptTarget() networkid.MessageID {
	return s.ID
}

func (s *SlackReadReceipt) GetReceiptTargets() []networkid.MessageID {
	return nil
}

func (s *SlackReadReceipt) GetReadUpTo() time.Time {
	return s.Timestamp
}

type SlackTyping struct {
	*SlackEventMeta
}

var _ bridgev2.RemoteTyping = (*SlackTyping)(nil)

func (s *SlackTyping) GetTimeout() time.Duration {
	return 5 * time.Second
}

type SlackReaction struct {
	*SlackEventMeta
	TargetID networkid.MessageID
	EmojiID  networkid.EmojiID
	Emoji    string
	Meta     map[string]any
}

func (s *SlackReaction) GetTargetMessage() networkid.MessageID {
	return s.TargetID
}

func (s *SlackReaction) GetReactionEmoji() (string, networkid.EmojiID) {
	return s.Emoji, s.EmojiID
}

func (s *SlackReaction) GetRemovedEmojiID() networkid.EmojiID {
	return s.EmojiID
}

func (s *SlackReaction) GetReactionExtraContent() map[string]any {
	return s.Meta
}

var (
	_ bridgev2.RemoteReaction                 = (*SlackReaction)(nil)
	_ bridgev2.RemoteReactionRemove           = (*SlackReaction)(nil)
	_ bridgev2.RemoteReactionWithExtraContent = (*SlackReaction)(nil)
)

type SlackMessage struct {
	*SlackEventMeta
	Data   *slack.MessageEvent
	Client *SlackClient
}

var (
	_ bridgev2.RemoteMessage        = (*SlackMessage)(nil)
	_ bridgev2.RemoteEdit           = (*SlackMessage)(nil)
	_ bridgev2.RemoteMessageRemove  = (*SlackMessage)(nil)
	_ bridgev2.RemoteChatInfoChange = (*SlackMessage)(nil)
)

func (s *SlackMessage) GetType() bridgev2.RemoteEventType {
	switch s.Data.SubType {
	case slack.MsgSubTypeMessageChanged:
		return bridgev2.RemoteEventEdit
	case slack.MsgSubTypeMessageDeleted:
		return bridgev2.RemoteEventMessageRemove
	case slack.MsgSubTypeChannelTopic, slack.MsgSubTypeChannelPurpose, slack.MsgSubTypeChannelName,
		slack.MsgSubTypeGroupTopic, slack.MsgSubTypeGroupPurpose, slack.MsgSubTypeGroupName:
		return bridgev2.RemoteEventChatInfoChange
	case slack.MsgSubTypeMessageReplied, slack.MsgSubTypeGroupJoin, slack.MsgSubTypeGroupLeave,
		slack.MsgSubTypeChannelJoin, slack.MsgSubTypeChannelLeave:
		return bridgev2.RemoteEventUnknown
	default:
		return bridgev2.RemoteEventMessage
	}
}

func (s *SlackMessage) ConvertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI) (*bridgev2.ConvertedMessage, error) {
	return s.Client.Main.MsgConv.ToMatrix(ctx, portal, intent, s.Client.UserLogin, &s.Data.Msg), nil
}

func (s *SlackMessage) ConvertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message) (*bridgev2.ConvertedEdit, error) {
	//msg := s.Data.SubMessage
	//oldMsg := s.Data.PreviousMessage
	//TODO implement me
	panic("implement me")
}

func (s *SlackMessage) GetTimestamp() time.Time {
	switch s.Data.SubType {
	case slack.MsgSubTypeMessageChanged:
		return slackid.ParseSlackTimestamp(s.Data.SubMessage.Timestamp)
	case slack.MsgSubTypeMessageDeleted:
		return slackid.ParseSlackTimestamp(s.Data.DeletedTimestamp)
	default:
		return s.Timestamp
	}
}

func (s *SlackMessage) GetTargetMessage() networkid.MessageID {
	return s.ID
}

func (s *SlackMessage) GetChatInfoChange(ctx context.Context) (*bridgev2.ChatInfoChange, error) {
	switch s.Data.SubType {
	// TODO
	}
	return nil, nil
}
