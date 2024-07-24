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
	"strings"

	"github.com/slack-go/slack"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-slack/pkg/slackid"
)

var (
	_ bridgev2.IdentifierResolvingNetworkAPI = (*SlackClient)(nil)
	_ bridgev2.UserSearchingNetworkAPI       = (*SlackClient)(nil)
)

func (s *SlackClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("not logged in")
	}
	if strings.ContainsRune(identifier, '-') {
		var teamID string
		teamID, identifier = slackid.ParseUserID(networkid.UserID(identifier))
		if teamID != s.TeamID {
			return nil, fmt.Errorf("identifier does not match team")
		}
	} else {
		identifier = strings.ToUpper(identifier)
	}
	userInfo, err := s.Client.GetUserInfoContext(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	userID := slackid.MakeUserID(s.TeamID, userInfo.ID)
	ghost, err := s.Main.br.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	}
	var chatResp *bridgev2.CreateChatResponse
	if createChat {
		resp, _, _, err := s.Client.OpenConversationContext(ctx, &slack.OpenConversationParameters{
			ReturnIM: true,
			Users:    []string{userInfo.ID},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to open conversation: %w", err)
		}
		chatInfo, err := s.wrapChatInfo(ctx, resp, true)
		if err != nil {
			return nil, fmt.Errorf("failed to wrap chat info: %w", err)
		}
		chatResp = &bridgev2.CreateChatResponse{
			PortalKey:  s.makePortalKey(resp),
			PortalInfo: chatInfo,
		}
	}
	return &bridgev2.ResolveIdentifierResponse{
		Ghost:  ghost,
		UserID: userID,
		Chat:   chatResp,
	}, nil
}

func (s *SlackClient) SearchUsers(ctx context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("not logged in")
	}
	resp, err := s.Client.SearchUsersCacheContext(ctx, s.TeamID, query)
	if err != nil {
		return nil, err
	}
	results := make([]*bridgev2.ResolveIdentifierResponse, len(resp.Results))
	for i, user := range resp.Results {
		userID := slackid.MakeUserID(s.TeamID, user.ID)
		ghost, err := s.Main.br.GetGhostByID(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("failed to get ghost: %w", err)
		}
		results[i] = &bridgev2.ResolveIdentifierResponse{
			Ghost:    ghost,
			UserID:   userID,
			UserInfo: s.wrapUserInfo(user.ID, user, nil),
		}
	}
	return results, nil
}
