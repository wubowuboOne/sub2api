//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWebSearchGroupUpdateAndAuthSnapshot(t *testing.T) {
	group := &Group{ID: 42, Name: "group", Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: 1}
	repo := &groupRepoStubForAdmin{getByID: group}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &webSearchProbeRepo{accounts: []Account{webSearchTestAccount(7, "only")}}, authCacheInvalidator: invalidator}
	id := int64(7)
	updated, err := svc.UpdateGroup(context.Background(), 42, &UpdateGroupInput{WebSearchAccountID: &id})
	require.NoError(t, err)
	require.Equal(t, id, updated.WebSearchAccountID)
	require.Equal(t, []int64{42}, invalidator.groupIDs)
	keySvc := &APIKeyService{}
	snapshot := keySvc.snapshotFromAPIKey(context.Background(), &APIKey{ID: 1, User: &User{ID: 1}, Group: updated})
	require.NotNil(t, snapshot)
	require.Equal(t, id, snapshot.Group.WebSearchAccountID)
	require.Equal(t, id, keySvc.snapshotToAPIKey("key", snapshot).Group.WebSearchAccountID)
	// An unrelated edit must retain the binding without revalidating a disabled member.
	svc.accountRepo = nil
	updated, err = svc.UpdateGroup(context.Background(), 42, &UpdateGroupInput{Name: "renamed", WebSearchAccountID: &id})
	require.NoError(t, err)
	require.Equal(t, id, updated.WebSearchAccountID)
	id = 0
	updated, err = svc.UpdateGroup(context.Background(), 42, &UpdateGroupInput{WebSearchAccountID: &id})
	require.NoError(t, err)
	require.Zero(t, updated.WebSearchAccountID)
}
