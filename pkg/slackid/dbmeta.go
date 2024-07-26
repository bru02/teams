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

package slackid

import (
	"go.mau.fi/util/jsontime"
)

type PortalMetadata struct {
	// Only present for team portals, not channels
	TeamDomain string `json:"team_domain"`
}

type GhostMetadata struct {
	LastSync jsontime.Unix `json:"last_sync"`
}

type UserLoginMetadata struct {
	Email       string `json:"email"`
	Token       string `json:"token"`
	CookieToken string `json:"cookie_token,omitempty"`
}

type MessageMetadata struct {
	CaptionMerged bool `json:"caption_merged"`
}
