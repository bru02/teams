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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-slack/pkg/msgconv"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		"slack-invalid-auth":           "Invalid credentials, please log in again",
		"slack-user-removed-from-team": "You were removed from the Slack workspace",
		"slack-id-mismatch":            "Unexpected internal error: got different user ID",
	})
}

func makeSlackClient(log *zerolog.Logger, token, cookieToken string) *slack.Client {
	options := []slack.Option{
		slack.OptionLog(slackgoZerolog{Logger: log.With().Str("component", "slackgo").Logger()}),
		slack.OptionDebug(log.GetLevel() == zerolog.TraceLevel),
	}
	if cookieToken != "" {
		options = append(options, slack.OptionCookie("d", cookieToken))
	}
	return slack.New(token, options...)
}

func (s *SlackConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	teamID, userID := slackid.ParseUserLoginID(login.ID)
	token, ok := login.Metadata.Extra["token"].(string)
	var sc *SlackClient
	if !ok {
		sc = &SlackClient{Main: s, UserLogin: login, UserID: userID, TeamID: teamID}
	} else {
		cookieToken, _ := login.Metadata.Extra["cookie_token"].(string)
		client := makeSlackClient(&login.Log, token, cookieToken)
		sc = &SlackClient{
			Main:      s,
			UserLogin: login,
			Client:    client,
			RTM:       client.NewRTM(),
			UserID:    userID,
			TeamID:    teamID,
		}
	}
	teamPortalKey := networkid.PortalKey{ID: slackid.MakeTeamPortalID(teamID)}
	var err error
	sc.TeamPortal, err = s.br.UnlockedGetPortalByID(ctx, teamPortalKey, false)
	if err != nil {
		return fmt.Errorf("failed to get team portal: %w", err)
	}
	login.Client = sc
	return nil
}

type SlackClient struct {
	Main       *SlackConnector
	UserLogin  *bridgev2.UserLogin
	Client     *slack.Client
	RTM        *slack.RTM
	UserID     string
	TeamID     string
	BootResp   *slack.ClientBootResponse
	TeamPortal *bridgev2.Portal
}

var _ bridgev2.NetworkAPI = (*SlackClient)(nil)

var _ msgconv.SlackClientProvider = (*SlackClient)(nil)

func (s *SlackClient) GetClient() *slack.Client {
	return s.Client
}

func (s *SlackClient) Connect(ctx context.Context) error {
	bootResp, err := s.Client.ClientBootContext(ctx)
	if err != nil {
		if err.Error() == "user_removed_from_team" || err.Error() == "invalid_auth" {
			s.invalidateSession(ctx, status.BridgeState{
				StateEvent: status.StateBadCredentials,
				Error:      status.BridgeStateErrorCode(fmt.Sprintf("slack-%s", strings.ReplaceAll(err.Error(), "_", "-"))),
			})
		} else {
			s.UserLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateUnknownError,
				Error:      "slack-unknown-fetch-error",
				Message:    fmt.Sprintf("Unknown error from Slack: %s", err.Error()),
			})
		}
		return err
	}
	return s.connect(ctx, bootResp)
}

func (s *SlackClient) connect(ctx context.Context, bootResp *slack.ClientBootResponse) error {
	s.BootResp = bootResp
	err := s.syncTeamPortal(ctx)
	if err != nil {
		return err
	}
	go s.consumeEvents()
	go s.RTM.ManageConnection()
	go s.SyncEmojis(ctx)
	go s.SyncChannels(ctx)
	return nil
}

func (s *SlackClient) syncTeamPortal(ctx context.Context) error {
	info := s.getTeamInfo()
	if s.TeamPortal.MXID == "" {
		err := s.TeamPortal.CreateMatrixRoom(ctx, s.UserLogin, info)
		if err != nil {
			return err
		}
	} else {
		s.TeamPortal.UpdateInfo(ctx, info, s.UserLogin, nil, time.Time{})
	}
	return nil
}

func (s *SlackClient) SyncChannels(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	serverInfo := make(map[string]*slack.Channel)
	token, _ := s.UserLogin.Metadata.Extra["token"].(string)
	if !strings.HasPrefix(token, "xoxs") {
		totalLimit := s.Main.Config.Backfill.ConversationCount
		var cursor string
		log.Debug().Int("total_limit", totalLimit).Msg("Fetching conversation list for sync")
		for totalLimit > 0 {
			reqLimit := totalLimit
			if totalLimit > 200 {
				reqLimit = 100
			}
			channelsChunk, nextCursor, err := s.Client.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
				Types:  []string{"public_channel", "private_channel", "mpim", "im"},
				Limit:  reqLimit,
				Cursor: cursor,
			})
			if err != nil {
				log.Err(err).Msg("Failed to fetch conversations for sync")
				return
			}
			log.Debug().Int("chunk_size", len(channelsChunk)).Msg("Fetched chunk of conversations")
			for _, channel := range channelsChunk {
				// Skip non-"open" DMs
				if channel.IsIM && (channel.Latest == nil || channel.Latest.SubType == "") {
					continue
				}
				serverInfo[channel.ID] = &channel
			}
			if nextCursor == "" || len(channelsChunk) == 0 {
				break
			}
			totalLimit -= len(channelsChunk)
			cursor = nextCursor
		}
	}
	userPortals, err := s.UserLogin.Bridge.DB.UserPortal.GetAllForLogin(ctx, s.UserLogin.UserLogin)
	if err != nil {
		log.Err(err).Msg("Failed to fetch existing portals for sync")
		return
	}
	for _, userPortal := range userPortals {
		_, channelID := slackid.ParsePortalID(userPortal.Portal.ID)
		if channelID == "" {
			continue
		}
		portal, err := s.UserLogin.Bridge.GetExistingPortalByID(ctx, userPortal.Portal)
		if err != nil {
			log.Err(err).Stringer("portal_key", userPortal.Portal).Msg("Failed to get existing portal for sync")
			continue
		} else {
			if portal.MXID != "" {
				// Refetch metadata because the list output doesn't have enough info
				info, err := s.GetChatInfo(ctx, portal)
				if err != nil {
					log.Err(err).Stringer("portal_key", portal.PortalKey).Msg("Failed to fetch chat info for existing portal")
				} else {
					portal.UpdateInfo(ctx, info, s.UserLogin, nil, time.Time{})
				}
				delete(serverInfo, channelID)
			}
		}
	}
	remainingChannels := make([]*slack.Channel, len(serverInfo))
	i := 0
	for _, channel := range serverInfo {
		remainingChannels[i] = channel
		i++
	}
	slices.SortFunc(remainingChannels, func(a, b *slack.Channel) int {
		return cmp.Compare(a.LastRead, b.LastRead)
	})
	for _, ch := range remainingChannels {
		portal, err := s.UserLogin.Bridge.GetPortalByID(ctx, s.makePortalKey(ch))
		if err != nil {
			log.Err(err).Str("channel_id", ch.ID).Msg("Failed to get portal for channel")
			continue
		}
		err = portal.CreateMatrixRoom(ctx, s.UserLogin, nil)
		if err != nil {
			log.Err(err).Str("channel_id", ch.ID).Msg("Failed to create Matrix room for channel")
		}
	}
}

func (s *SlackClient) consumeEvents() {
	for evt := range s.RTM.IncomingEvents {
		s.HandleSlackEvent(evt.Data)
	}
}

func (s *SlackClient) Disconnect() {
	if rtm := s.RTM; rtm != nil {
		err := rtm.Disconnect()
		if err != nil {
			s.UserLogin.Log.Err(err).Msg("Failed to disconnect RTM")
		}
		// TODO stop consumeEvents?
		s.RTM = nil
	}
	s.Client = nil
}

func (s *SlackClient) IsLoggedIn() bool {
	return s.Client != nil
}

func (s *SlackClient) LogoutRemote(ctx context.Context) {
	_, err := s.Client.SendAuthSignoutContext(ctx)
	if err != nil {
		s.UserLogin.Log.Err(err).Msg("Failed to send sign out request to Slack")
	}
}

func (s *SlackClient) invalidateSession(ctx context.Context, state status.BridgeState) {
	s.UserLogin.Metadata.Extra["token"] = ""
	s.UserLogin.Metadata.Extra["cookie_token"] = ""
	err := s.UserLogin.Save(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to save user login after invalidating session")
	}
	s.Disconnect()
	s.UserLogin.BridgeState.Send(state)
}

func (s *SlackClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	return slackid.UserIDToUserLoginID(userID) == s.UserLogin.ID
}
