// Copyright 2023 Harness, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package githook

import (
	"context"
	"fmt"
	"strings"

	"github.com/harness/gitness/app/auth"
	events "github.com/harness/gitness/app/events/git"
	"github.com/harness/gitness/githook"
	"github.com/harness/gitness/types"
	"github.com/harness/gitness/types/enum"

	"github.com/rs/zerolog/log"
)

const (
	// gitReferenceNamePrefixBranch is the prefix of references of type branch.
	gitReferenceNamePrefixBranch = "refs/heads/"

	// gitReferenceNamePrefixTag is the prefix of references of type tag.
	gitReferenceNamePrefixTag = "refs/tags/"
)

// PostReceive executes the post-receive hook for a git repository.
func (c *Controller) PostReceive(
	ctx context.Context,
	session *auth.Session,
	repoID int64,
	principalID int64,
	in *githook.PostReceiveInput,
) (*githook.Output, error) {
	if in == nil {
		return nil, fmt.Errorf("input is nil")
	}

	repo, err := c.getRepoCheckAccess(ctx, session, repoID, enum.PermissionRepoEdit)
	if err != nil {
		return nil, err
	}

	// report ref events (best effort)
	c.reportReferenceEvents(ctx, repoID, principalID, in)

	// create output object and have following messages fill its messages
	out := &githook.Output{}

	// handle branch updates related to PRs - best effort
	c.handlePRMessaging(ctx, repo, in, out)

	return out, nil
}

// reportReferenceEvents is reporting reference events to the event system.
// NOTE: keep best effort for now as it doesn't change the outcome of the git operation.
// TODO: in the future we might want to think about propagating errors so user is aware of events not being triggered.
func (c *Controller) reportReferenceEvents(
	ctx context.Context,
	repoID int64,
	principalID int64,
	in *githook.PostReceiveInput,
) {
	for _, refUpdate := range in.RefUpdates {
		switch {
		case strings.HasPrefix(refUpdate.Ref, gitReferenceNamePrefixBranch):
			c.reportBranchEvent(ctx, repoID, principalID, refUpdate)
		case strings.HasPrefix(refUpdate.Ref, gitReferenceNamePrefixTag):
			c.reportTagEvent(ctx, repoID, principalID, refUpdate)
		default:
			// Ignore any other references in post-receive
		}
	}
}

func (c *Controller) reportBranchEvent(
	ctx context.Context,
	repoID int64,
	principalID int64,
	branchUpdate githook.ReferenceUpdate,
) {
	switch {
	case branchUpdate.Old == types.NilSHA:
		c.gitReporter.BranchCreated(ctx, &events.BranchCreatedPayload{
			RepoID:      repoID,
			PrincipalID: principalID,
			Ref:         branchUpdate.Ref,
			SHA:         branchUpdate.New,
		})
	case branchUpdate.New == types.NilSHA:
		c.gitReporter.BranchDeleted(ctx, &events.BranchDeletedPayload{
			RepoID:      repoID,
			PrincipalID: principalID,
			Ref:         branchUpdate.Ref,
			SHA:         branchUpdate.Old,
		})
	default:
		c.gitReporter.BranchUpdated(ctx, &events.BranchUpdatedPayload{
			RepoID:      repoID,
			PrincipalID: principalID,
			Ref:         branchUpdate.Ref,
			OldSHA:      branchUpdate.Old,
			NewSHA:      branchUpdate.New,
			Forced:      false, // TODO: data not available yet
		})
	}
}

func (c *Controller) reportTagEvent(
	ctx context.Context,
	repoID int64,
	principalID int64,
	tagUpdate githook.ReferenceUpdate,
) {
	switch {
	case tagUpdate.Old == types.NilSHA:
		c.gitReporter.TagCreated(ctx, &events.TagCreatedPayload{
			RepoID:      repoID,
			PrincipalID: principalID,
			Ref:         tagUpdate.Ref,
			SHA:         tagUpdate.New,
		})
	case tagUpdate.New == types.NilSHA:
		c.gitReporter.TagDeleted(ctx, &events.TagDeletedPayload{
			RepoID:      repoID,
			PrincipalID: principalID,
			Ref:         tagUpdate.Ref,
			SHA:         tagUpdate.Old,
		})
	default:
		c.gitReporter.TagUpdated(ctx, &events.TagUpdatedPayload{
			RepoID:      repoID,
			PrincipalID: principalID,
			Ref:         tagUpdate.Ref,
			OldSHA:      tagUpdate.Old,
			NewSHA:      tagUpdate.New,
			// tags can only be force updated!
			Forced: true,
		})
	}
}

// handlePRMessaging checks any single branch push for pr information and returns an according response if needed.
// TODO: If it is a new branch, or an update on a branch without any PR, it also sends out an SSE for pr creation.
func (c *Controller) handlePRMessaging(
	ctx context.Context,
	repo *types.Repository,
	in *githook.PostReceiveInput,
	out *githook.Output,
) {
	// skip anything that was a batch push / isn't branch related / isn't updating/creating a branch.
	if len(in.RefUpdates) != 1 ||
		!strings.HasPrefix(in.RefUpdates[0].Ref, gitReferenceNamePrefixBranch) ||
		in.RefUpdates[0].New == types.NilSHA {
		return
	}

	// for now we only care about first branch that was pushed.
	branchName := in.RefUpdates[0].Ref[len(gitReferenceNamePrefixBranch):]

	// do we have a PR related to it?
	prs, err := c.pullreqStore.List(ctx, &types.PullReqFilter{
		Page: 1,
		// without forks we expect at most one PR (keep 2 to not break when forks are introduced)
		Size:         2,
		SourceRepoID: repo.ID,
		SourceBranch: branchName,
		// we only care about open PRs - merged/closed will lead to "create new PR" message
		States: []enum.PullReqState{enum.PullReqStateOpen},
		Order:  enum.OrderAsc,
		Sort:   enum.PullReqSortCreated,
	})
	if err != nil {
		log.Ctx(ctx).Warn().Err(err).Msgf(
			"failed to find pullrequests for branch '%s' originating from repo '%s'",
			branchName,
			repo.Path,
		)
		return
	}

	// for already existing PRs, print them to users terminal for easier access.
	if len(prs) > 0 {
		msgs := make([]string, 2*len(prs)+1)
		msgs[0] = fmt.Sprintf("Branch '%s' has open PRs:", branchName)
		for i, pr := range prs {
			msgs[2*i+1] = fmt.Sprintf("  (#%d) %s", pr.Number, pr.Title)
			msgs[2*i+2] = "    " + c.urlProvider.GenerateUIPRURL(repo.Path, pr.Number)
		}
		out.Messages = append(out.Messages, msgs...)
		return
	}

	// this is a new PR!
	out.Messages = append(out.Messages,
		fmt.Sprintf("Create a new PR for branch '%s'", branchName),
		"  "+c.urlProvider.GenerateUICompareURL(repo.Path, repo.DefaultBranch, branchName),
	)

	// TODO: store latest pushed branch for user in cache and send out SSE
}
