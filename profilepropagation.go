// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package mautrix

import (
	"context"
	"net/url"
)

// ProfilePropagation says which rooms should get a new member event when the global display name
// or avatar changes (MSC4466).
type ProfilePropagation string

const (
	// ProfilePropagationAll rewrites the member event in every joined room, which is what a
	// homeserver does when it is not told anything.
	ProfilePropagationAll ProfilePropagation = "all"
	// ProfilePropagationUnchanged skips the rooms where the member event already differs from the
	// old global value, so a name or avatar that was set for one room survives a global change.
	ProfilePropagationUnchanged ProfilePropagation = "unchanged"
	// ProfilePropagationNone changes only the global profile.
	ProfilePropagationNone ProfilePropagation = "none"
)

// The name the proposal's author gave the parameter while it is unstable.
const profilePropagationParam = "computer.gingershaped.msc4466.propagate_to"

type profilePropagationKey struct{}

// WithProfilePropagation makes SetDisplayName, SetAvatarURL and SetProfileField calls using the
// returned context ask the homeserver for the given propagation mode.
//
// A homeserver that does not know the parameter ignores it and rewrites every room, so the caller
// still has to cope with that.
func WithProfilePropagation(ctx context.Context, mode ProfilePropagation) context.Context {
	return context.WithValue(ctx, profilePropagationKey{}, mode)
}

// addProfilePropagation adds the mode asked for in the context to a profile URL that has no query
// yet.
func addProfilePropagation(ctx context.Context, urlPath string) string {
	mode, _ := ctx.Value(profilePropagationKey{}).(ProfilePropagation)
	if mode == "" {
		return urlPath
	}
	return urlPath + "?" + url.Values{profilePropagationParam: {string(mode)}}.Encode()
}
