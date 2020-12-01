package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	minio "github.com/minio/minio-go"
	globlib "github.com/pachyderm/ohmyglob"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/auth"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/client/pps"
	authserver "github.com/pachyderm/pachyderm/src/server/auth/server"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	tu "github.com/pachyderm/pachyderm/src/server/pkg/testutil"
)

// aclEntry mirrors auth.ACLEntry, but without XXX_fields, which make
// auth.ACLEntry invalid to use as a map key
type aclEntry struct {
	Username string
	Scope    auth.Scope
}

func key(e *auth.ACLEntry) aclEntry {
	return aclEntry{
		Username: e.Username,
		Scope:    e.Scope,
	}
}

// entries constructs an auth.ACL struct from a list of the form
// [ principal_1, scope_1, principal_2, scope_2, ... ]. All unlabelled
// principals are assumed to be GitHub users
func entries(items ...string) []aclEntry {
	if len(items)%2 != 0 {
		panic("cannot create an ACL from an odd number of items")
	}
	if len(items) == 0 {
		return []aclEntry{}
	}
	result := make([]aclEntry, 0, len(items)/2)
	for i := 0; i < len(items); i += 2 {
		scope, err := auth.ParseScope(items[i+1])
		if err != nil {
			panic(fmt.Sprintf("could not parse scope: %v", err))
		}
		principal := items[i]
		if !strings.Contains(principal, ":") {
			principal = auth.GitHubPrefix + principal
		}
		result = append(result, aclEntry{Username: principal, Scope: scope})
	}
	return result
}

// GetACL uses the client 'c' to get the ACL protecting the repo 'repo'
// TODO(msteffen) create an auth client?
func getACL(t *testing.T, c *client.APIClient, repo string) []aclEntry {
	t.Helper()
	resp, err := c.AuthAPIClient.GetACL(c.Ctx(), &auth.GetACLRequest{
		Repo: repo,
	})
	require.NoError(t, err)
	result := make([]aclEntry, 0, len(resp.Entries))
	for _, p := range resp.Entries {
		result = append(result, key(p))
	}
	return result
}

// CommitCnt uses 'c' to get the number of commits made to the repo 'repo'
func CommitCnt(t *testing.T, c *client.APIClient, repo string) int {
	t.Helper()
	commitList, err := c.ListCommitByRepo(repo)
	require.NoError(t, err)
	return len(commitList)
}

// PipelineNames returns the names of all pipelines that 'c' gets from
// ListPipeline
func PipelineNames(t *testing.T, c *client.APIClient) []string {
	t.Helper()
	ps, err := c.ListPipeline()
	require.NoError(t, err)
	result := make([]string, len(ps))
	for i, p := range ps {
		result[i] = p.Pipeline.Name
	}
	return result
}

// TestGetSetBasic creates two users, alice and bob, and gives bob gradually
// escalating privileges, checking what bob can and can't do after each change
func TestGetSetBasic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// create repo, and check that alice is the owner of the new repo
	dataRepo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(dataRepo))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))

	// Add data to repo (alice can write). Make sure alice can read also.
	_, err := aliceClient.PutFile(dataRepo, "master", "/file", strings.NewReader("1"))
	require.NoError(t, err)
	buf := &bytes.Buffer{}
	require.NoError(t, aliceClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())

	//////////
	/// Initially, bob has no privileges
	// bob can't read
	err = bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// bob can't write (check both the standalone form of PutFile and StartCommit)
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("lorem ipsum"))
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.Equal(t, 1, CommitCnt(t, aliceClient, dataRepo)) // check that no commits were created
	_, err = bobClient.StartCommit(dataRepo, "master")
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.Equal(t, 1, CommitCnt(t, aliceClient, dataRepo)) // check that no commits were created
	// bob can't update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// check that ACL wasn't updated)
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))

	//////////
	/// alice adds bob to the ACL as a reader (alice can modify ACL)
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())
	// bob can't write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("2"))
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.Equal(t, 1, CommitCnt(t, aliceClient, dataRepo)) // check that no commits were created
	_, err = bobClient.StartCommit(dataRepo, "master")
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.Equal(t, 1, CommitCnt(t, aliceClient, dataRepo)) // check that no commits were created
	// bob can't update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// check that ACL wasn't updated)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader"), getACL(t, aliceClient, dataRepo))

	//////////
	/// alice adds bob to the ACL as a writer
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())
	// bob can write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("2"))
	require.NoError(t, err)
	require.Equal(t, 2, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	commit, err := bobClient.StartCommit(dataRepo, "master")
	require.NoError(t, err)
	require.NoError(t, bobClient.FinishCommit(dataRepo, commit.ID))
	require.Equal(t, 3, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	// bob can't update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// check that ACL wasn't updated)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "writer"), getACL(t, aliceClient, dataRepo))

	//////////
	/// alice adds bob to the ACL as an owner
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_OWNER,
	})
	require.NoError(t, err)
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "12", buf.String())
	// bob can write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("3"))
	require.NoError(t, err)
	require.Equal(t, 4, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	commit, err = bobClient.StartCommit(dataRepo, "master")
	require.NoError(t, err)
	require.NoError(t, bobClient.FinishCommit(dataRepo, commit.ID))
	require.Equal(t, 5, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	// bob can update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	// check that ACL was updated)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "owner", "carol", "reader"),
		getACL(t, aliceClient, dataRepo))
}

// TestGetSetReverse creates two users, alice and bob, and gives bob gradually
// shrinking privileges, checking what bob can and can't do after each change
func TestGetSetReverse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// create repo, and check that alice is the owner of the new repo
	dataRepo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(dataRepo))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))

	// Add data to repo (alice can write). Make sure alice can read also.
	commit, err := aliceClient.StartCommit(dataRepo, "master")
	require.NoError(t, err)
	_, err = aliceClient.PutFile(dataRepo, commit.ID, "/file", strings.NewReader("1"))
	require.NoError(t, err)
	require.NoError(t, aliceClient.FinishCommit(dataRepo, commit.ID)) // # commits = 1
	buf := &bytes.Buffer{}
	require.NoError(t, aliceClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())

	//////////
	/// alice adds bob to the ACL as an owner
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_OWNER,
	})
	require.NoError(t, err)
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())
	// bob can write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("2"))
	require.NoError(t, err)
	require.Equal(t, 2, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	commit, err = bobClient.StartCommit(dataRepo, "master")
	require.NoError(t, err)
	require.NoError(t, bobClient.FinishCommit(dataRepo, commit.ID))
	require.Equal(t, 3, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	// bob can update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	// check that ACL was updated)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "owner", "carol", "reader"),
		getACL(t, aliceClient, dataRepo))

	// clear carol
	aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_NONE,
	})
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "owner"), getACL(t, aliceClient, dataRepo))

	//////////
	/// alice adds bob to the ACL as a writer
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "12", buf.String())
	// bob can write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("3"))
	require.NoError(t, err)
	require.Equal(t, 4, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	commit, err = bobClient.StartCommit(dataRepo, "master")
	require.NoError(t, err)
	require.NoError(t, bobClient.FinishCommit(dataRepo, commit.ID))
	require.Equal(t, 5, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	// bob can't update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// check that ACL wasn't updated)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "writer"), getACL(t, aliceClient, dataRepo))

	//////////
	/// alice adds bob to the ACL as a reader (alice can modify ACL)
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "123", buf.String())
	// bob can't write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("4"))
	require.YesError(t, err)
	require.Equal(t, 5, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	_, err = bobClient.StartCommit(dataRepo, "master")
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.Equal(t, 5, CommitCnt(t, aliceClient, dataRepo)) // check that no commits were created
	// bob can't update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// check that ACL wasn't updated)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader"), getACL(t, aliceClient, dataRepo))

	//////////
	/// alice revokes all of bob's privileges
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_NONE,
	})
	require.NoError(t, err)
	// bob can't read
	err = bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// bob can't write
	_, err = bobClient.PutFile(dataRepo, "master", "/file", strings.NewReader("4"))
	require.YesError(t, err)
	require.Equal(t, 5, CommitCnt(t, aliceClient, dataRepo)) // check that a new commit was created
	_, err = bobClient.StartCommit(dataRepo, "master")
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.Equal(t, 5, CommitCnt(t, aliceClient, dataRepo)) // check that no commits were created
	// bob can't update the ACL
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: "carol",
		Scope:    auth.Scope_READER,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	// check that ACL wasn't updated)
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))
}

// TestCreateAndUpdateRepo tests that if CreateRepo(foo, update=true) is
// called, and foo exists, then the ACL for foo won't be modified.
func TestCreateAndUpdateRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// create repo, and check that alice is the owner of the new repo
	dataRepo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(dataRepo))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))

	// Add data to repo (alice can write). Make sure alice can read also.
	_, err := aliceClient.PutFile(dataRepo, "master", "/file", strings.NewReader("1"))
	require.NoError(t, err)
	buf := &bytes.Buffer{}
	require.NoError(t, aliceClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())

	/// alice adds bob to the ACL as a reader (alice can modify ACL)
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader"), getACL(t, aliceClient, dataRepo))
	// bob can read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())

	/// alice updates the repo
	description := "This request updates the description to force a write"
	_, err = aliceClient.PfsAPIClient.CreateRepo(aliceClient.Ctx(), &pfs.CreateRepoRequest{
		Repo:        client.NewRepo(dataRepo),
		Description: description,
		Update:      true,
	})
	require.NoError(t, err)
	repoInfo, err := aliceClient.InspectRepo(dataRepo)
	require.NoError(t, err)
	require.Equal(t, description, repoInfo.Description)
	// entries haven't changed
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader"), getACL(t, aliceClient, dataRepo))
	// bob can still read
	buf.Reset()
	require.NoError(t, bobClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())
}

// TestCreateRepoWithUpdateFlag tests that if CreateRepo(foo, update=true) is
// called, and foo doesn't exist, then the ACL for foo will still be created and
// initialized to the correct value
func TestCreateRepoWithUpdateFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// create repo, and check that alice is the owner of the new repo
	dataRepo := tu.UniqueString(t.Name())
	/// alice creates the repo with Update set
	_, err := aliceClient.PfsAPIClient.CreateRepo(aliceClient.Ctx(), &pfs.CreateRepoRequest{
		Repo:   client.NewRepo(dataRepo),
		Update: true,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))

	// Add data to repo (alice can write). Make sure alice can read also.
	_, err = aliceClient.PutFile(dataRepo, "master", "/file", strings.NewReader("1"))
	require.NoError(t, err)
	buf := &bytes.Buffer{}
	require.NoError(t, aliceClient.GetFile(dataRepo, "master", "/file", 0, 0, buf))
	require.Equal(t, "1", buf.String())
}

func TestCreateAndUpdatePipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	type createArgs struct {
		client     *client.APIClient
		name, repo string
		update     bool
	}
	createPipeline := func(args createArgs) error {
		return args.client.CreatePipeline(
			args.name,
			"", // default image: ubuntu:16.04
			[]string{"bash"},
			[]string{"cp /pfs/*/* /pfs/out/"},
			&pps.ParallelismSpec{Constant: 1},
			client.NewPFSInput(args.repo, "/*"),
			"", // default output branch: master
			args.update,
		)
	}
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// create repo, and check that alice is the owner of the new repo
	dataRepo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(dataRepo))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo))

	// alice can create a pipeline (she owns the input repo)
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, createPipeline(createArgs{
		client: aliceClient,
		name:   pipeline,
		repo:   dataRepo,
	}))
	require.OneOfEquals(t, pipeline, PipelineNames(t, aliceClient))
	// check that alice owns the output repo too)
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "writer"), getACL(t, aliceClient, pipeline))

	// Make sure alice's pipeline runs successfully
	_, err := aliceClient.PutFile(dataRepo, "master", tu.UniqueString("/file"),
		strings.NewReader("test data"))
	require.NoError(t, err)
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(dataRepo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		_, err := iter.Next()
		return err
	})

	// bob can't create a pipeline
	badPipeline := tu.UniqueString("bob-bad")
	err = createPipeline(createArgs{
		client: bobClient,
		name:   badPipeline,
		repo:   dataRepo,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.NoneEquals(t, badPipeline, PipelineNames(t, aliceClient))

	// alice adds bob as a reader of the input repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)

	// now bob can create a pipeline
	goodPipeline := tu.UniqueString("bob-good")
	require.NoError(t, createPipeline(createArgs{
		client: bobClient,
		name:   goodPipeline,
		repo:   dataRepo,
	}))
	require.OneOfEquals(t, goodPipeline, PipelineNames(t, aliceClient))
	// check that bob owns the output repo too)
	require.ElementsEqual(t,
		entries(bob, "owner", pl(goodPipeline), "writer"), getACL(t, bobClient, goodPipeline))

	// Make sure bob's pipeline runs successfully
	_, err = aliceClient.PutFile(dataRepo, "master", tu.UniqueString("/file"),
		strings.NewReader("test data"))
	require.NoError(t, err)
	iter, err = bobClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(dataRepo, "master")},
		[]*pfs.Repo{client.NewRepo(goodPipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		_, err := iter.Next()
		return err
	})
	require.NoError(t, err)

	// bob can't update alice's pipeline
	infoBefore, err := aliceClient.InspectPipeline(pipeline)
	require.NoError(t, err)
	err = createPipeline(createArgs{
		client: bobClient,
		name:   pipeline,
		repo:   dataRepo,
		update: true,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	infoAfter, err := aliceClient.InspectPipeline(pipeline)
	require.NoError(t, err)
	require.Equal(t, infoBefore.Version, infoAfter.Version)

	// alice adds bob as a writer of the output repo, and removes him as a reader
	// of the input repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     pipeline,
		Username: bob,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "writer", pl(pipeline), "writer"),
		getACL(t, aliceClient, pipeline))

	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_NONE,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader", pl(goodPipeline), "reader"),
		getACL(t, aliceClient, dataRepo))

	// bob still can't update alice's pipeline
	infoBefore, err = aliceClient.InspectPipeline(pipeline)
	require.NoError(t, err)
	err = createPipeline(createArgs{
		client: bobClient,
		name:   pipeline,
		repo:   dataRepo,
		update: true,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	infoAfter, err = aliceClient.InspectPipeline(pipeline)
	require.NoError(t, err)
	require.Equal(t, infoBefore.Version, infoAfter.Version)

	// alice re-adds bob as a reader of the input repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader", pl(pipeline), "reader", pl(goodPipeline), "reader"),
		getACL(t, aliceClient, dataRepo))

	// now bob can update alice's pipeline
	infoBefore, err = aliceClient.InspectPipeline(pipeline)
	require.NoError(t, err)
	err = createPipeline(createArgs{
		client: bobClient,
		name:   pipeline,
		repo:   dataRepo,
		update: true,
	})
	require.NoError(t, err)
	infoAfter, err = aliceClient.InspectPipeline(pipeline)
	require.NoError(t, err)
	require.NotEqual(t, infoBefore.Version, infoAfter.Version)

	// Make sure the updated pipeline runs successfully
	_, err = aliceClient.PutFile(dataRepo, "master", tu.UniqueString("/file"),
		strings.NewReader("test data"))
	require.NoError(t, err)
	iter, err = bobClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(dataRepo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		_, err := iter.Next()
		return err
	})
}

func TestPipelineMultipleInputs(t *testing.T) {
	if os.Getenv("RUN_BAD_TESTS") == "" {
		t.Skip("Skipping because RUN_BAD_TESTS was empty")
	}
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	type createArgs struct {
		client *client.APIClient
		name   string
		input  *pps.Input
		update bool
	}
	createPipeline := func(args createArgs) error {
		return args.client.CreatePipeline(
			args.name,
			"", // default image: ubuntu:16.04
			[]string{"bash"},
			[]string{"echo \"work\" >/pfs/out/x"},
			&pps.ParallelismSpec{Constant: 1},
			args.input,
			"", // default output branch: master
			args.update,
		)
	}
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// create two repos, and check that alice is the owner of the new repos
	dataRepo1 := tu.UniqueString(t.Name())
	dataRepo2 := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(dataRepo1))
	require.NoError(t, aliceClient.CreateRepo(dataRepo2))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo1))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, dataRepo2))

	// alice can create a cross-pipeline with both inputs
	aliceCrossPipeline := tu.UniqueString("alice-cross")
	require.NoError(t, createPipeline(createArgs{
		client: aliceClient,
		name:   aliceCrossPipeline,
		input: client.NewCrossInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
	}))
	require.OneOfEquals(t, aliceCrossPipeline, PipelineNames(t, aliceClient))
	// check that alice owns the output repo too)
	require.ElementsEqual(t,
		entries(alice, "owner", pl(aliceCrossPipeline), "writer"), getACL(t, aliceClient, aliceCrossPipeline))

	// alice can create a union-pipeline with both inputs
	aliceUnionPipeline := tu.UniqueString("alice-union")
	require.NoError(t, createPipeline(createArgs{
		client: aliceClient,
		name:   aliceUnionPipeline,
		input: client.NewUnionInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
	}))
	require.OneOfEquals(t, aliceUnionPipeline, PipelineNames(t, aliceClient))
	// check that alice owns the output repo too)
	require.ElementsEqual(t,
		entries(alice, "owner", pl(aliceUnionPipeline), "writer"), getACL(t, aliceClient, aliceUnionPipeline))

	// alice adds bob as a reader of one of the input repos, but not the other
	_, err := aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo1,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)

	// bob cannot create a cross-pipeline with both inputs
	bobCrossPipeline := tu.UniqueString("bob-cross")
	err = createPipeline(createArgs{
		client: bobClient,
		name:   bobCrossPipeline,
		input: client.NewCrossInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.NoneEquals(t, bobCrossPipeline, PipelineNames(t, aliceClient))

	// bob cannot create a union-pipeline with both inputs
	bobUnionPipeline := tu.UniqueString("bob-union")
	err = createPipeline(createArgs{
		client: bobClient,
		name:   bobUnionPipeline,
		input: client.NewUnionInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	require.NoneEquals(t, bobUnionPipeline, PipelineNames(t, aliceClient))

	// alice adds bob as a writer of her pipeline's output
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     aliceCrossPipeline,
		Username: bob,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)

	// bob can update alice's pipeline if he removes one of the inputs
	infoBefore, err := aliceClient.InspectPipeline(aliceCrossPipeline)
	require.NoError(t, err)
	require.NoError(t, createPipeline(createArgs{
		client: bobClient,
		name:   aliceCrossPipeline,
		input: client.NewCrossInput(
			// This cross input deliberately only has one element, to make sure it's
			// not simply rejected for having a cross input
			client.NewPFSInput(dataRepo1, "/*"),
		),
		update: true,
	}))
	infoAfter, err := aliceClient.InspectPipeline(aliceCrossPipeline)
	require.NoError(t, err)
	require.NotEqual(t, infoBefore.Version, infoAfter.Version)

	// bob cannot update alice's to put the second input back
	infoBefore, err = aliceClient.InspectPipeline(aliceCrossPipeline)
	require.NoError(t, err)
	err = createPipeline(createArgs{
		client: bobClient,
		name:   aliceCrossPipeline,
		input: client.NewCrossInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
		update: true,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	infoAfter, err = aliceClient.InspectPipeline(aliceCrossPipeline)
	require.NoError(t, err)
	require.Equal(t, infoBefore.Version, infoAfter.Version)

	// alice adds bob as a reader of the second input
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     dataRepo2,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)

	// bob can now update alice's to put the second input back
	infoBefore, err = aliceClient.InspectPipeline(aliceCrossPipeline)
	require.NoError(t, err)
	require.NoError(t, createPipeline(createArgs{
		client: bobClient,
		name:   aliceCrossPipeline,
		input: client.NewCrossInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
		update: true,
	}))
	infoAfter, err = aliceClient.InspectPipeline(aliceCrossPipeline)
	require.NoError(t, err)
	require.NotEqual(t, infoBefore.Version, infoAfter.Version)

	// bob can create a cross-pipeline with both inputs
	require.NoError(t, createPipeline(createArgs{
		client: bobClient,
		name:   bobCrossPipeline,
		input: client.NewCrossInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
	}))
	require.OneOfEquals(t, bobCrossPipeline, PipelineNames(t, aliceClient))

	// bob can create a union-pipeline with both inputs
	require.NoError(t, createPipeline(createArgs{
		client: bobClient,
		name:   bobUnionPipeline,
		input: client.NewUnionInput(
			client.NewPFSInput(dataRepo1, "/*"),
			client.NewPFSInput(dataRepo2, "/*"),
		),
	}))
	require.OneOfEquals(t, bobUnionPipeline, PipelineNames(t, aliceClient))

}

// TestPipelineRevoke tests revoking the privileges of a pipeline's creator as
// well as revoking the pipeline itself.
//
// When pipelines inherited privileges from their creator, revoking the owner's
// access to the pipeline's inputs would cause pipelines to stop running, but
// now it does not. In general, this should actually be more secure--it used to
// be that any pipeline Bob created could access any repo that Bob could, even
// if the repo was unrelated to the pipeline (making pipelines a powerful
// vector for privilege escalation). Now pipelines are their own principals,
// and they can only read from their inputs and write to their outputs.
//
// Ideally both would be required: if either the pipeline's access to its inputs
// or bob's access to the pipeline's inputs are revoked, the pipeline should
// stop, but for now it's required to revoke the pipeline's access directly
func TestPipelineRevoke(t *testing.T) {
	t.Skip("TestPipelineRevoke is broken")
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo, and adds bob as a reader
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	_, err := aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader"), getACL(t, aliceClient, repo))

	// bob creates a pipeline
	pipeline := tu.UniqueString("bob-pipeline")
	require.NoError(t, bobClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))
	require.ElementsEqual(t,
		entries(bob, "owner", pl(pipeline), "writer"), getACL(t, bobClient, pipeline))
	// bob adds alice as a reader of the pipeline's output repo, so alice can
	// flush input commits (which requires her to inspect commits in the output)
	// and update the pipeline
	_, err = bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     pipeline,
		Username: alice,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(bob, "owner", alice, "writer", pl(pipeline), "writer"),
		getACL(t, bobClient, pipeline))

	// alice commits to the input repo, and the pipeline runs successfully
	require.NoError(t, err)
	_, err = aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)
	iter, err := bobClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 45*time.Second, func() error {
		_, err := iter.Next()
		return err
	})

	// alice removes bob as a reader of her repo, and then commits to the input
	// repo, but bob's pipeline still runs (it has its own principal--it doesn't
	// inherit bob's privileges)
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: bob,
		Scope:    auth.Scope_NONE,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo))
	_, err = aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)
	iter, err = aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 45*time.Second, func() error {
		_, err := iter.Next()
		return err
	})

	// alice revokes the pipeline's access to 'repo' directly, and the pipeline
	// stops running
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: pl(pipeline),
		Scope:    auth.Scope_NONE,
	})
	_, err = aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		iter, err = aliceClient.FlushCommit(
			[]*pfs.Commit{client.NewCommit(repo, "master")},
			[]*pfs.Repo{client.NewRepo(pipeline)},
		)
		require.NoError(t, err)
		_, err = iter.Next()
		require.NoError(t, err)
	}()
	select {
	case <-doneCh:
		t.Fatal("pipeline should not be able to finish with no access")
	case <-time.After(45 * time.Second):
	}

	// alice updates bob's pipline, but the pipeline still doesn't run
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		true,
	))
	_, err = aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)
	doneCh = make(chan struct{})
	go func() {
		defer close(doneCh)
		iter, err = aliceClient.FlushCommit(
			[]*pfs.Commit{client.NewCommit(repo, "master")},
			[]*pfs.Repo{client.NewRepo(pipeline)},
		)
		require.NoError(t, err)
		_, err = iter.Next()
		require.NoError(t, err)
	}()
	select {
	case <-doneCh:
		t.Fatal("pipeline should not be able to finish with no access")
	case <-time.After(45 * time.Second):
	}

	// alice restores the pipeline's access to its input repo, and now the
	// pipeline runs successfully
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: pl(pipeline),
		Scope:    auth.Scope_READER,
	})
	iter, err = aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 45*time.Second, func() error {
		for { // flushCommit yields two output commits (one from the prev pipeline)
			_, err = iter.Next()
			if errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
		}
		return nil
	})
}

func TestStopAndDeletePipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo))

	// alice creates a pipeline
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))
	// Make sure the input and output repos have non-empty ACLs
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "writer"), getACL(t, aliceClient, pipeline))

	// alice stops the pipeline (owner of the input and output repos can stop)
	require.NoError(t, aliceClient.StopPipeline(pipeline))

	// Make sure the remaining input and output repos *still* have non-empty ACLs
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "writer"), getACL(t, aliceClient, pipeline))

	// alice deletes the pipeline (owner of the input and output repos can delete)
	require.NoError(t, aliceClient.DeletePipeline(pipeline, false))
	require.ElementsEqual(t, entries(), getACL(t, aliceClient, pipeline))

	// alice deletes the input repo (make sure the input repo's ACL is gone)
	require.NoError(t, aliceClient.DeleteRepo(repo, false))
	require.ElementsEqual(t, entries(), getACL(t, aliceClient, repo))

	// alice creates another repo
	repo = tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo))

	// alice creates another pipeline
	pipeline = tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))

	// bob can't stop or delete alice's pipeline
	err := bobClient.StopPipeline(pipeline)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	err = bobClient.DeletePipeline(pipeline, false)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())

	// alice adds bob as a reader of the input repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader", pl(pipeline), "reader"),
		getACL(t, aliceClient, repo))

	// bob still can't stop or delete alice's pipeline
	err = bobClient.StopPipeline(pipeline)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	err = bobClient.DeletePipeline(pipeline, false)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())

	// alice removes bob as a reader of the input repo and adds bob as a writer of
	// the output repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: bob,
		Scope:    auth.Scope_NONE,
	})
	require.NoError(t, err)

	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo))
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     pipeline,
		Username: bob,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "writer", pl(pipeline), "writer"),
		getACL(t, aliceClient, pipeline))

	// bob still can't stop or delete alice's pipeline
	err = bobClient.StopPipeline(pipeline)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
	err = bobClient.DeletePipeline(pipeline, false)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())

	// alice re-adds bob as a reader of the input repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader", pl(pipeline), "reader"),
		getACL(t, aliceClient, repo))

	// bob can stop (and start) but not delete alice's pipeline
	err = bobClient.StopPipeline(pipeline)
	require.NoError(t, err)
	err = bobClient.StartPipeline(pipeline)
	require.NoError(t, err)
	err = bobClient.DeletePipeline(pipeline, false)
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())

	// alice adds bob as an owner of the output repo
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     pipeline,
		Username: bob,
		Scope:    auth.Scope_OWNER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "owner", pl(pipeline), "writer"),
		getACL(t, aliceClient, pipeline))

	// finally bob can stop and delete alice's pipeline
	err = bobClient.StopPipeline(pipeline)
	require.NoError(t, err)
	err = bobClient.DeletePipeline(pipeline, false)
	require.NoError(t, err)
}

// TestStopJob just confirms that the StopJob API works when auth is on
func TestStopJob(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo))
	_, err := aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)

	// alice creates a pipeline
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"sleep 600"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))
	// Make sure the input and output repos have non-empty ACLs
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "writer"), getACL(t, aliceClient, pipeline))

	// Stop the first job in 'pipeline'
	var jobID string
	require.NoErrorWithinTRetry(t, 30*time.Second, func() error {
		jobs, err := aliceClient.ListJob(pipeline, nil /*inputs*/, nil /*output*/, -1 /*history*/, true /* full */)
		if err != nil {
			return err
		}
		if len(jobs) != 1 {
			return errors.Errorf("expected one job but got %d", len(jobs))
		}
		jobID = jobs[0].Job.ID
		return nil
	})

	require.NoError(t, aliceClient.StopJob(jobID))
	require.NoErrorWithinTRetry(t, 30*time.Second, func() error {
		ji, err := aliceClient.InspectJob(jobID, false)
		if err != nil {
			return errors.Wrapf(err, "could not inspect job %q", jobID)
		}
		if ji.State != pps.JobState_JOB_KILLED {
			return errors.Errorf("expected job %q to be in JOB_KILLED but was in %s", jobID, ji.State.String())
		}
		return nil
	})
}

// Test ListRepo checks that the auth information returned by ListRepo and
// InspectRepo is correct.
// TODO(msteffen): This should maybe go in pachyderm_test, since ListRepo isn't
// an auth API call
func TestListAndInspectRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo and makes Bob a writer
	repoWriter := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repoWriter))
	_, err := aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repoWriter,
		Username: bob,
		Scope:    auth.Scope_WRITER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "writer"), getACL(t, aliceClient, repoWriter))

	// alice creates a repo and makes Bob a reader
	repoReader := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repoReader))
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repoReader,
		Username: bob,
		Scope:    auth.Scope_READER,
	})
	require.NoError(t, err)
	require.ElementsEqual(t,
		entries(alice, "owner", bob, "reader"), getACL(t, aliceClient, repoReader))

	// alice creates a repo and gives Bob no access privileges
	repoNone := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repoNone))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, repoNone))

	// bob creates a repo
	repoOwner := tu.UniqueString(t.Name())
	require.NoError(t, bobClient.CreateRepo(repoOwner))
	require.ElementsEqual(t, entries(bob, "owner"), getACL(t, bobClient, repoOwner))

	// Bob calls ListRepo, and the response must indicate the correct access scope
	// for each repo (because other tests have run, we may see repos besides the
	// above. Bob's access to those should be NONE
	listResp, err := bobClient.PfsAPIClient.ListRepo(bobClient.Ctx(),
		&pfs.ListRepoRequest{})
	require.NoError(t, err)
	expectedAccess := map[string]auth.Scope{
		repoOwner:  auth.Scope_OWNER,
		repoWriter: auth.Scope_WRITER,
		repoReader: auth.Scope_READER,
	}
	for _, info := range listResp.RepoInfo {
		require.Equal(t, expectedAccess[info.Repo.Name], info.AuthInfo.AccessLevel)
	}

	for _, name := range []string{repoOwner, repoWriter, repoReader, repoNone} {
		inspectResp, err := bobClient.PfsAPIClient.InspectRepo(bobClient.Ctx(),
			&pfs.InspectRepoRequest{
				Repo: &pfs.Repo{Name: name},
			})
		require.NoError(t, err)
		require.Equal(t, expectedAccess[name], inspectResp.AuthInfo.AccessLevel)
	}
}

func TestUnprivilegedUserCannotMakeSelfOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, repo))

	// bob calls SetScope(bob, OWNER) on alice's repo. This should fail
	_, err := bobClient.SetScope(bobClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Scope:    auth.Scope_OWNER,
		Username: bob,
	})
	require.YesError(t, err)
	// make sure ACL wasn't updated
	require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo))
}

func TestGetScopeRequiresReader(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo))

	// bob calls GetScope(repo). This should succeed
	resp, err := bobClient.GetScope(bobClient.Ctx(), &auth.GetScopeRequest{
		Repos: []string{repo},
	})
	require.NoError(t, err)
	require.Equal(t, []auth.Scope{auth.Scope_NONE}, resp.Scopes)

	// bob calls GetScope(repo, alice). This should fail because bob isn't a
	// READER
	_, err = bobClient.GetScope(bobClient.Ctx(),
		&auth.GetScopeRequest{
			Repos:    []string{repo},
			Username: alice,
		})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
}

// TestListRepoNotLoggedInError makes sure that if a user isn't logged in, and
// they call ListRepo(), they get an error.
func TestListRepoNotLoggedInError(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient, anonClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, "")

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, repo))

	// Anon (non-logged-in user) calls ListRepo, and must receive an error
	_, err := anonClient.PfsAPIClient.ListRepo(anonClient.Ctx(),
		&pfs.ListRepoRequest{})
	require.YesError(t, err)
	require.Matches(t, "no authentication token", err.Error())
}

// TestListRepoNoAuthInfoIfDeactivated tests that if auth isn't activated, then
// ListRepo returns RepoInfos where AuthInfo isn't set (i.e. is nil)
func TestListRepoNoAuthInfoIfDeactivated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	// Dont't run this test in parallel, since it deactivates the auth system
	// globally, so any tests running concurrently will fail
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)
	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// bob calls ListRepo, but has NONE access to all repos
	infos, err := bobClient.ListRepo()
	require.NoError(t, err)
	for _, info := range infos {
		require.Equal(t, auth.Scope_NONE, info.AuthInfo.AccessLevel)
	}

	// Deactivate auth
	_, err = adminClient.Deactivate(adminClient.Ctx(), &auth.DeactivateRequest{})
	require.NoError(t, err)

	// Wait for auth to be deactivated
	require.NoError(t, backoff.Retry(func() error {
		_, err := aliceClient.WhoAmI(aliceClient.Ctx(), &auth.WhoAmIRequest{})
		if err != nil && auth.IsErrNotActivated(err) {
			return nil // WhoAmI should fail when auth is deactivated
		}
		return errors.New("auth is not yet deactivated")
	}, backoff.NewTestingBackOff()))

	// bob calls ListRepo, now AuthInfo isn't set anywhere
	infos, err = bobClient.ListRepo()
	require.NoError(t, err)
	for _, info := range infos {
		require.Nil(t, info.AuthInfo)
	}
}

// TestCreateRepoAlreadyExistsError tests that creating a repo that already
// exists gives you an error to that effect, even when auth is already
// activated (rather than "access denied")
func TestCreateRepoAlreadyExistsError(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// bob creates the same repo, and should get an error to the effect that the
	// repo already exists (rather than "access denied")
	err := bobClient.CreateRepo(repo)
	require.YesError(t, err)
	require.Matches(t, "already exists", err.Error())
}

// TestCreateRepoNotLoggedInError makes sure that if a user isn't logged in, and
// they call CreateRepo(), they get an error.
func TestCreateRepoNotLoggedInError(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	anonClient := tu.GetAuthenticatedPachClient(t, "")

	// anonClient tries and fails to create a repo
	repo := tu.UniqueString(t.Name())
	err := anonClient.CreateRepo(repo)
	require.YesError(t, err)
	require.Matches(t, "no authentication token", err.Error())
}

// Creating a pipeline when the output repo already exists gives you an error to
// that effect, even when auth is already activated (rather than "access
// denied")
func TestCreatePipelineRepoAlreadyExistsError(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	inputRepo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(inputRepo))
	aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     inputRepo,
	})
	pipeline := tu.UniqueString("pipeline")
	require.NoError(t, aliceClient.CreateRepo(pipeline))

	// bob creates a pipeline, and should get an error to the effect that the
	// repo already exists (rather than "access denied")
	err := bobClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(inputRepo, "/*"),
		"",    // default output branch: master
		false, // Don't update -- we want an error
	)
	require.YesError(t, err)
	require.Matches(t, "cannot overwrite repo", err.Error())
}

// TestAuthorizedNoneRole tests that Authorized(user, repo, NONE) yields 'true',
// even for repos with no ACL
func TestAuthorizedNoneRole(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// Deactivate auth
	_, err := adminClient.Deactivate(adminClient.Ctx(), &auth.DeactivateRequest{})
	require.NoError(t, err)

	// Wait for auth to be deactivated
	require.NoError(t, backoff.Retry(func() error {
		_, err = adminClient.WhoAmI(adminClient.Ctx(), &auth.WhoAmIRequest{})
		if err != nil && auth.IsErrNotActivated(err) {
			return nil // WhoAmI should fail when auth is deactivated
		}
		return errors.New("auth is not yet deactivated")
	}, backoff.NewTestingBackOff()))

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, adminClient.CreateRepo(repo))

	// Get new pach clients, re-activating auth
	alice := tu.UniqueString("alice")
	aliceClient, adminClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// Check that the repo has no ACL
	require.ElementsEqual(t, entries(), getACL(t, adminClient, repo))

	// alice authorizes against it with the 'NONE' scope
	resp, err := aliceClient.Authorize(aliceClient.Ctx(), &auth.AuthorizeRequest{
		Repo:  repo,
		Scope: auth.Scope_NONE,
	})
	require.NoError(t, err)
	require.True(t, resp.Authorized)
}

// TestAuthorizedEveryone tests that Authorized(user, repo, NONE) tests that the
// `allClusterUsers` binding  for an ACL sets the minimum authorized scope
func TestAuthorizedEveryone(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// alice is authorized as `OWNER`
	resp, err := aliceClient.Authorize(aliceClient.Ctx(), &auth.AuthorizeRequest{
		Repo:  repo,
		Scope: auth.Scope_OWNER,
	})
	require.NoError(t, err)
	require.True(t, resp.Authorized)

	// bob is not authorized
	resp, err = bobClient.Authorize(bobClient.Ctx(), &auth.AuthorizeRequest{
		Repo:  repo,
		Scope: auth.Scope_READER,
	})
	require.NoError(t, err)
	require.False(t, resp.Authorized)

	// alice grants everybody WRITER access
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Repo:     repo,
		Scope:    auth.Scope_WRITER,
		Username: "allClusterUsers",
	})
	require.NoError(t, err)

	// alice is still authorized as `OWNER`
	resp, err = aliceClient.Authorize(aliceClient.Ctx(), &auth.AuthorizeRequest{
		Repo:  repo,
		Scope: auth.Scope_OWNER,
	})
	require.NoError(t, err)
	require.True(t, resp.Authorized)

	// bob is now authorized as WRITER
	resp, err = bobClient.Authorize(bobClient.Ctx(), &auth.AuthorizeRequest{
		Repo:  repo,
		Scope: auth.Scope_WRITER,
	})
	require.NoError(t, err)
	require.True(t, resp.Authorized)
}

// TestDeleteAll tests that you must be a cluster admin to call DeleteAll
func TestDeleteAll(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	aliceClient, adminClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, adminClient.CreateRepo(repo))

	// alice calls DeleteAll, but it fails
	err := aliceClient.DeleteAll()
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())

	// admin makes alice an fs admin
	_, err = adminClient.ModifyClusterRoleBinding(adminClient.Ctx(),
		&auth.ModifyClusterRoleBindingRequest{Principal: gh(alice), Roles: fsClusterRole()})
	require.NoError(t, err)

	// wait until alice shows up in admin list
	require.NoError(t, backoff.Retry(func() error {
		resp, err := aliceClient.GetClusterRoleBindings(aliceClient.Ctx(), &auth.GetClusterRoleBindingsRequest{})
		require.NoError(t, err)
		return require.EqualOrErr(
			admins(tu.AdminUser)(gh(alice)), resp.Bindings,
		)
	}, backoff.NewTestingBackOff()))

	// alice calls DeleteAll but it fails because she's only an fs admin
	err = aliceClient.DeleteAll()
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())

	// admin calls DeleteAll and succeeds
	require.NoError(t, adminClient.DeleteAll())
}

// TestListDatum tests that you must have READER access to all of job's
// input repos to call ListDatum on that job
func TestListDatum(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repoA := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repoA))
	repoB := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repoB))

	// alice creates a pipeline
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"ls /pfs/*/*; cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewCrossInput(
			client.NewPFSInput(repoA, "/*"),
			client.NewPFSInput(repoB, "/*"),
		),
		"", // default output branch: master
		false,
	))

	// alice commits to the input repos, and the pipeline runs successfully
	for i, repo := range []string{repoA, repoB} {
		var err error
		file := fmt.Sprintf("/file%d", i+1)
		_, err = aliceClient.PutFile(repo, "master", file, strings.NewReader("test"))
		require.NoError(t, err)
	}
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repoB, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 45*time.Second, func() error {
		_, err := iter.Next()
		return err
	})
	jobs, err := aliceClient.ListJob(pipeline, nil /*inputs*/, nil /*output*/, -1 /*history*/, true /* full */)
	require.NoError(t, err)
	require.Equal(t, 2, len(jobs))
	jobID := jobs[0].Job.ID

	// bob cannot call ListDatum
	_, err = bobClient.ListDatum(jobID, 0 /*pageSize*/, 0 /*page*/)
	require.YesError(t, err)
	require.True(t, auth.IsErrNotAuthorized(err), err.Error())

	// alice adds bob to repoA, but bob still can't call GetLogs
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     repoA,
	})
	require.NoError(t, err)
	_, err = bobClient.ListDatum(jobID, 0 /*pageSize*/, 0 /*page*/)
	require.YesError(t, err)
	require.True(t, auth.IsErrNotAuthorized(err), err.Error())

	// alice removes bob from repoA and adds bob to repoB, but bob still can't
	// call ListDatum
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_NONE,
		Repo:     repoA,
	})
	require.NoError(t, err)
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     repoB,
	})
	require.NoError(t, err)
	_, err = bobClient.ListDatum(jobID, 0 /*pageSize*/, 0 /*page*/)
	require.YesError(t, err)
	require.True(t, auth.IsErrNotAuthorized(err), err.Error())

	// alice adds bob to repoA, and now bob can call ListDatum
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     repoA,
	})
	require.NoError(t, err)
	_, err = bobClient.ListDatum(jobID, 0 /*pageSize*/, 0 /*page*/)
	require.YesError(t, err)
	require.True(t, auth.IsErrNotAuthorized(err), err.Error())

	// Finally, alice adds bob to the output repo, and now bob can call ListDatum
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     pipeline,
	})
	require.NoError(t, err)
	resp, err := bobClient.ListDatum(jobID, 0 /*pageSize*/, 0 /*page*/)
	require.NoError(t, err)
	files := make(map[string]struct{})
	for _, di := range resp.DatumInfos {
		for _, f := range di.Data {
			files[path.Base(f.File.Path)] = struct{}{}
		}
	}
	require.Equal(t, map[string]struct{}{
		"file1": struct{}{},
		"file2": struct{}{},
	}, files)
}

// TestListJob tests that you must have READER access to a pipeline's output
// repo to call ListJob on that pipeline, but a blank ListJob always succeeds
// (but doesn't return a given job if you don't have access to the job's output
// repo)
func TestListJob(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// alice creates a pipeline
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"ls /pfs/*/*; cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))

	// alice commits to the input repos, and the pipeline runs successfully
	var err error
	_, err = aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		_, err := iter.Next()
		return err
	})
	jobs, err := aliceClient.ListJob(pipeline, nil /*inputs*/, nil /*output*/, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs))
	jobID := jobs[0].Job.ID

	// bob cannot call ListJob on 'pipeline'
	_, err = bobClient.ListJob(pipeline, nil, nil, -1 /*history*/, true)
	require.YesError(t, err)
	require.True(t, auth.IsErrNotAuthorized(err), err.Error())
	// bob can call blank ListJob, but gets no results
	jobs, err = bobClient.ListJob("", nil, nil, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 0, len(jobs))

	// alice adds bob to repo, but bob still can't call ListJob on 'pipeline' or
	// get any output
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     repo,
	})
	require.NoError(t, err)
	_, err = bobClient.ListJob(pipeline, nil, nil, -1 /*history*/, true)
	require.YesError(t, err)
	require.True(t, auth.IsErrNotAuthorized(err), err.Error())
	jobs, err = bobClient.ListJob("", nil, nil, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 0, len(jobs))

	// alice removes bob from repo and adds bob to 'pipeline', and now bob can
	// call listJob on 'pipeline', and gets results back from blank listJob
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_NONE,
		Repo:     repo,
	})
	require.NoError(t, err)
	_, err = aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     pipeline,
	})
	require.NoError(t, err)
	jobs, err = bobClient.ListJob(pipeline, nil, nil, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs))
	require.Equal(t, jobID, jobs[0].Job.ID)
	jobs, err = bobClient.ListJob("", nil, nil, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs))
	require.Equal(t, jobID, jobs[0].Job.ID)
}

// TestInspectDatum tests InspectDatum runs even when auth is activated
func TestInspectDatum(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// alice creates a pipeline (we must enable stats for InspectDatum, which
	// means calling the grpc client function directly)
	pipeline := tu.UniqueString("alice-pipeline")
	_, err := aliceClient.PpsAPIClient.CreatePipeline(aliceClient.Ctx(),
		&pps.CreatePipelineRequest{
			Pipeline: &pps.Pipeline{Name: pipeline},
			Transform: &pps.Transform{
				Cmd:   []string{"bash"},
				Stdin: []string{"cp /pfs/*/* /pfs/out/"},
			},
			ParallelismSpec: &pps.ParallelismSpec{Constant: 1},
			Input:           client.NewPFSInput(repo, "/*"),
			EnableStats:     true,
		})
	require.NoError(t, err)

	// alice commits to the input repo, and the pipeline runs successfully
	_, err = aliceClient.PutFile(repo, "master", "/file", strings.NewReader("test"))
	require.NoError(t, err)
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		_, err := iter.Next()
		return err
	})
	jobs, err := aliceClient.ListJob(pipeline, nil /*inputs*/, nil /*output*/, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs))
	jobID := jobs[0].Job.ID

	// ListDatum seems like it may return inconsistent results, so sleep until
	// the /stats branch is written
	// TODO(msteffen): verify if this is true, and if so, why
	time.Sleep(5 * time.Second)
	resp, err := aliceClient.ListDatum(jobID, 0 /*pageSize*/, 0 /*page*/)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		for _, di := range resp.DatumInfos {
			if _, err := aliceClient.InspectDatum(jobID, di.Datum.ID); err != nil {
				continue
			}
		}
		return nil
	})
}

// TestGetLogs tests that you must have READER access to all of a job's input
// repos and READER access to its output repo to call GetLogs()
func TestGetLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient, bobClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, bob)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// alice creates a pipeline
	pipeline := tu.UniqueString("pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))

	// alice commits to the input repos, and the pipeline runs successfully
	_, err := aliceClient.PutFile(repo, "master", "/file1", strings.NewReader("test"))
	require.NoError(t, err)
	commitIter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 60*time.Second, func() error {
		_, err := commitIter.Next()
		return err
	})

	// bob cannot call GetLogs
	iter := bobClient.GetLogs(pipeline, "", nil, "", false, false, 0)
	require.False(t, iter.Next())
	require.YesError(t, iter.Err())
	require.True(t, auth.IsErrNotAuthorized(iter.Err()), iter.Err().Error())

	// bob also can't call GetLogs for the master process
	iter = bobClient.GetLogs(pipeline, "", nil, "", true, false, 0)
	require.False(t, iter.Next())
	require.YesError(t, iter.Err())
	require.True(t, auth.IsErrNotAuthorized(iter.Err()), iter.Err().Error())

	// alice adds bob to the input repo, but bob still can't call GetLogs
	aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     repo,
	})
	iter = bobClient.GetLogs(pipeline, "", nil, "", false, false, 0)
	require.False(t, iter.Next())
	require.YesError(t, iter.Err())
	require.True(t, auth.IsErrNotAuthorized(iter.Err()), iter.Err().Error())

	// alice removes bob from the input repo and adds bob to the output repo, but
	// bob still can't call GetLogs
	aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_NONE,
		Repo:     repo,
	})
	aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     pipeline,
	})
	iter = bobClient.GetLogs(pipeline, "", nil, "", false, false, 0)
	require.False(t, iter.Next())
	require.YesError(t, iter.Err())
	require.True(t, auth.IsErrNotAuthorized(iter.Err()), iter.Err().Error())

	// alice adds bob to the output repo, and now bob can call GetLogs
	aliceClient.SetScope(aliceClient.Ctx(), &auth.SetScopeRequest{
		Username: bob,
		Scope:    auth.Scope_READER,
		Repo:     repo,
	})
	iter = bobClient.GetLogs(pipeline, "", nil, "", false, false, 0)
	iter.Next()
	require.NoError(t, iter.Err())

	// bob can also call GetLogs for the master process
	iter = bobClient.GetLogs(pipeline, "", nil, "", true, false, 0)
	iter.Next()
	require.NoError(t, iter.Err())
}

// TestGetLogsFromStats tests that GetLogs still works even when stats are
// enabled
func TestGetLogsFromStats(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))

	// alice creates a pipeline (we must enable stats for InspectDatum, which
	// means calling the grpc client function directly)
	pipeline := tu.UniqueString("alice")
	_, err := aliceClient.PpsAPIClient.CreatePipeline(aliceClient.Ctx(),
		&pps.CreatePipelineRequest{
			Pipeline: &pps.Pipeline{Name: pipeline},
			Transform: &pps.Transform{
				Cmd:   []string{"bash"},
				Stdin: []string{"cp /pfs/*/* /pfs/out/"},
			},
			ParallelismSpec: &pps.ParallelismSpec{Constant: 1},
			Input:           client.NewPFSInput(repo, "/*"),
			EnableStats:     true,
		})
	require.NoError(t, err)

	// alice commits to the input repo, and the pipeline runs successfully
	_, err = aliceClient.PutFile(repo, "master", "/file1", strings.NewReader("test"))
	require.NoError(t, err)
	commitItr, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	require.NoErrorWithinT(t, 3*time.Minute, func() error {
		_, err := commitItr.Next()
		return err
	})
	jobs, err := aliceClient.ListJob(pipeline, nil /*inputs*/, nil /*output*/, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs))
	jobID := jobs[0].Job.ID

	iter := aliceClient.GetLogs("", jobID, nil, "", false, false, 0)
	require.True(t, iter.Next())
	require.NoError(t, iter.Err())

	iter = aliceClient.GetLogs("", jobID, nil, "", true, false, 0)
	iter.Next()
	require.NoError(t, iter.Err())
}

func TestPipelineNewInput(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// alice creates three repos and commits to them
	var repo []string
	for i := 0; i < 3; i++ {
		repo = append(repo, tu.UniqueString(fmt.Sprint("TestPipelineNewInput-", i, "-")))
		require.NoError(t, aliceClient.CreateRepo(repo[i]))
		require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo[i]))

		// Commit to repo
		_, err := aliceClient.PutFile(
			repo[i], "master", "/"+repo[i], strings.NewReader(repo[i]))
		require.NoError(t, err)
	}

	// alice creates a pipeline
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewUnionInput(
			client.NewPFSInput(repo[0], "/*"),
			client.NewPFSInput(repo[1], "/*"),
		),
		"", // default output branch: master
		false,
	))
	// Make sure the input and output repos have appropriate ACLs
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo[0]))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo[1]))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "writer"), getACL(t, aliceClient, pipeline))
	// repo[2] is not on pipeline -- doesn't include 'pipeline'
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, repo[2]))

	// make sure the pipeline runs
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo[0], "master")}, nil)
	require.NoError(t, err)
	require.NoErrorWithinT(t, time.Minute, func() error {
		_, err := iter.Next()
		return err
	})

	// alice updates the pipeline to replace repo[0] with repo[2]
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewUnionInput(
			client.NewPFSInput(repo[1], "/*"),
			client.NewPFSInput(repo[2], "/*"),
		),
		"", // default output branch: master
		true,
	))
	// Make sure the input and output repos have appropriate ACLs
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo[1]))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "reader"), getACL(t, aliceClient, repo[2]))
	require.ElementsEqual(t,
		entries(alice, "owner", pl(pipeline), "writer"), getACL(t, aliceClient, pipeline))
	// repo[0] is not on pipeline -- doesn't include 'pipeline'
	require.ElementsEqual(t,
		entries(alice, "owner"), getACL(t, aliceClient, repo[0]))

	// make sure the pipeline still runs
	iter, err = aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo[2], "master")}, nil)
	require.NoError(t, err)
	require.NoErrorWithinT(t, time.Minute, func() error {
		_, err := iter.Next()
		return err
	})
}

func TestModifyMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	bob := tu.UniqueString("bob")
	organization := tu.UniqueString("organization")
	engineering := tu.UniqueString("engineering")
	security := tu.UniqueString("security")

	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// This is a sequence dependent list of tests
	tests := []struct {
		Requests []*auth.ModifyMembersRequest
		Expected map[string][]string
	}{
		{
			[]*auth.ModifyMembersRequest{
				&auth.ModifyMembersRequest{
					Add:   []string{alice},
					Group: organization,
				},
				&auth.ModifyMembersRequest{
					Add:   []string{alice},
					Group: organization,
				},
			},
			map[string][]string{
				alice: []string{organization},
			},
		},
		{
			[]*auth.ModifyMembersRequest{
				&auth.ModifyMembersRequest{
					Add:   []string{bob},
					Group: organization,
				},
				&auth.ModifyMembersRequest{
					Add:   []string{alice, bob},
					Group: engineering,
				},
				&auth.ModifyMembersRequest{
					Add:   []string{bob},
					Group: security,
				},
			},
			map[string][]string{
				alice: []string{organization, engineering},
				bob:   []string{organization, engineering, security},
			},
		},
		{
			[]*auth.ModifyMembersRequest{
				&auth.ModifyMembersRequest{
					Add:    []string{alice},
					Remove: []string{bob},
					Group:  security,
				},
				&auth.ModifyMembersRequest{
					Remove: []string{bob},
					Group:  engineering,
				},
			},
			map[string][]string{
				alice: []string{organization, engineering, security},
				bob:   []string{organization},
			},
		},
		{
			[]*auth.ModifyMembersRequest{
				&auth.ModifyMembersRequest{
					Remove: []string{alice, bob},
					Group:  organization,
				},
				&auth.ModifyMembersRequest{
					Remove: []string{alice, bob},
					Group:  security,
				},
				&auth.ModifyMembersRequest{
					Add:    []string{alice},
					Remove: []string{alice},
					Group:  organization,
				},
				&auth.ModifyMembersRequest{
					Add:    []string{},
					Remove: []string{},
					Group:  organization,
				},
			},
			map[string][]string{
				alice: []string{engineering},
				bob:   []string{},
			},
		},
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			for _, req := range test.Requests {
				_, err := adminClient.ModifyMembers(adminClient.Ctx(), req)
				require.NoError(t, err)
			}

			for username, groups := range test.Expected {
				groupsActual, err := adminClient.GetGroups(adminClient.Ctx(), &auth.GetGroupsRequest{
					Username: username,
				})
				require.NoError(t, err)
				require.ElementsEqual(t, groups, groupsActual.Groups)

				for _, group := range groups {
					users, err := adminClient.GetUsers(adminClient.Ctx(), &auth.GetUsersRequest{
						Group: group,
					})
					require.NoError(t, err)
					require.OneOfEquals(t, gh(username), users.Usernames)
				}
			}
		})
	}
}

func TestSetGroupsForUser(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	organization := tu.UniqueString("organization")
	engineering := tu.UniqueString("engineering")
	security := tu.UniqueString("security")

	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	groups := []string{organization, engineering}
	_, err := adminClient.SetGroupsForUser(adminClient.Ctx(), &auth.SetGroupsForUserRequest{
		Username: alice,
		Groups:   groups,
	})
	require.NoError(t, err)
	groupsActual, err := adminClient.GetGroups(adminClient.Ctx(), &auth.GetGroupsRequest{
		Username: alice,
	})
	require.NoError(t, err)
	require.ElementsEqual(t, groups, groupsActual.Groups)
	for _, group := range groups {
		users, err := adminClient.GetUsers(adminClient.Ctx(), &auth.GetUsersRequest{
			Group: group,
		})
		require.NoError(t, err)
		require.OneOfEquals(t, gh(alice), users.Usernames)
	}

	groups = append(groups, security)
	_, err = adminClient.SetGroupsForUser(adminClient.Ctx(), &auth.SetGroupsForUserRequest{
		Username: alice,
		Groups:   groups,
	})
	require.NoError(t, err)
	groupsActual, err = adminClient.GetGroups(adminClient.Ctx(), &auth.GetGroupsRequest{
		Username: alice,
	})
	require.NoError(t, err)
	require.ElementsEqual(t, groups, groupsActual.Groups)
	for _, group := range groups {
		users, err := adminClient.GetUsers(adminClient.Ctx(), &auth.GetUsersRequest{
			Group: group,
		})
		require.NoError(t, err)
		require.OneOfEquals(t, gh(alice), users.Usernames)
	}

	groups = groups[:1]
	_, err = adminClient.SetGroupsForUser(adminClient.Ctx(), &auth.SetGroupsForUserRequest{
		Username: alice,
		Groups:   groups,
	})
	require.NoError(t, err)
	groupsActual, err = adminClient.GetGroups(adminClient.Ctx(), &auth.GetGroupsRequest{
		Username: alice,
	})
	require.NoError(t, err)
	require.ElementsEqual(t, groups, groupsActual.Groups)
	for _, group := range groups {
		users, err := adminClient.GetUsers(adminClient.Ctx(), &auth.GetUsersRequest{
			Group: group,
		})
		require.NoError(t, err)
		require.OneOfEquals(t, gh(alice), users.Usernames)
	}

	groups = []string{}
	_, err = adminClient.SetGroupsForUser(adminClient.Ctx(), &auth.SetGroupsForUserRequest{
		Username: alice,
		Groups:   groups,
	})
	require.NoError(t, err)
	groupsActual, err = adminClient.GetGroups(adminClient.Ctx(), &auth.GetGroupsRequest{
		Username: alice,
	})
	require.NoError(t, err)
	require.ElementsEqual(t, groups, groupsActual.Groups)
	for _, group := range groups {
		users, err := adminClient.GetUsers(adminClient.Ctx(), &auth.GetUsersRequest{
			Group: group,
		})
		require.NoError(t, err)
		require.OneOfEquals(t, gh(alice), users.Usernames)
	}
}

func TestGetGroupsEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	organization := tu.UniqueString("organization")
	engineering := tu.UniqueString("engineering")
	security := tu.UniqueString("security")

	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	_, err := adminClient.SetGroupsForUser(adminClient.Ctx(), &auth.SetGroupsForUserRequest{
		Username: alice,
		Groups:   []string{organization, engineering, security},
	})
	require.NoError(t, err)

	aliceClient := tu.GetAuthenticatedPachClient(t, alice)
	groups, err := aliceClient.GetGroups(aliceClient.Ctx(), &auth.GetGroupsRequest{})
	require.NoError(t, err)
	require.ElementsEqual(t, []string{organization, engineering, security}, groups.Groups)

	groups, err = adminClient.GetGroups(adminClient.Ctx(), &auth.GetGroupsRequest{})
	require.NoError(t, err)
	require.Equal(t, 0, len(groups.Groups))
}

// TestGetJobsBugFix tests the fix for https://github.com/pachyderm/pachyderm/issues/2879
// where calling pps.ListJob when not logged in would delete all old jobs
func TestGetJobsBugFix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient, anonClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, "")

	// alice creates a repo
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	require.ElementsEqual(t, entries(alice, "owner"), getACL(t, aliceClient, repo))
	_, err := aliceClient.PutFile(repo, "master", "/file", strings.NewReader("lorem ipsum"))
	require.NoError(t, err)

	// alice creates a pipeline
	pipeline := tu.UniqueString("alice-pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"", // default image: ubuntu:16.04
		[]string{"bash"},
		[]string{"cp /pfs/*/* /pfs/out/"},
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))

	// Wait for pipeline to finish
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)},
	)
	require.NoError(t, err)
	_, err = iter.Next()
	require.NoError(t, err)

	// alice calls 'list job'
	jobs, err := aliceClient.ListJob("", nil, nil, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs))

	// anonClient calls 'list job'
	_, err = anonClient.ListJob("", nil, nil, -1 /*history*/, true)
	require.YesError(t, err)
	require.Matches(t, "no authentication token", err.Error())

	// alice calls 'list job' again, and the existing job must still be present
	jobs2, err := aliceClient.ListJob("", nil, nil, -1 /*history*/, true)
	require.NoError(t, err)
	require.Equal(t, 1, len(jobs2))
	require.Equal(t, jobs[0].Job.ID, jobs2[0].Job.ID)
}

// TestGetAuthTokenNoSubject tests that calling GetAuthToken without the subject
// explicitly set to the calling user works, even if the caller isn't an admin.
func TestGetAuthTokenNoSubject(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	aliceClient, anonClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, "")

	// Get GetOTP with no subject
	resp, err := aliceClient.GetAuthToken(aliceClient.Ctx(), &auth.GetAuthTokenRequest{})
	require.NoError(t, err)
	anonClient.SetAuthToken(resp.Token)
	who, err := anonClient.WhoAmI(anonClient.Ctx(), &auth.WhoAmIRequest{})
	require.NoError(t, err)
	require.Equal(t, gh(alice), who.Username)
}

// TestGetOneTimePasswordNoSubject tests that calling GetOneTimePassword without
// the subject explicitly set to the calling user works, even if the caller
// isn't an admin.
func TestOneTimePasswordNoSubject(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	aliceClient, anonClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, "")

	// Get GetOTP with no subject
	otpResp, err := aliceClient.GetOneTimePassword(aliceClient.Ctx(),
		&auth.GetOneTimePasswordRequest{})
	require.NoError(t, err)

	authResp, err := anonClient.Authenticate(anonClient.Ctx(), &auth.AuthenticateRequest{
		OneTimePassword: otpResp.Code,
	})
	require.NoError(t, err)
	anonClient.SetAuthToken(authResp.PachToken)
	who, err := anonClient.WhoAmI(anonClient.Ctx(), &auth.WhoAmIRequest{})
	require.NoError(t, err)
	require.Equal(t, gh(alice), who.Username)
}

// TestOneTimePassword tests the GetOneTimePassword -> Authenticate auth flow
func TestGetOneTimePassword(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	aliceClient, anonClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, "")

	// Get GetOTP with subject equal to the caller
	otpResp, err := aliceClient.GetOneTimePassword(aliceClient.Ctx(),
		&auth.GetOneTimePasswordRequest{Subject: alice})
	require.NoError(t, err)

	anonClient.SetAuthToken("")
	authResp, err := anonClient.Authenticate(anonClient.Ctx(), &auth.AuthenticateRequest{
		OneTimePassword: otpResp.Code,
	})
	require.NoError(t, err)
	anonClient.SetAuthToken(authResp.PachToken)
	who, err := anonClient.WhoAmI(anonClient.Ctx(), &auth.WhoAmIRequest{})
	require.NoError(t, err)
	require.Equal(t, gh(alice), who.Username)
}

// TestOneTimePasswordOtherUserError tests that if a non-admin tries to
// generate an OTP on behalf of another user, they'll get an error
func TestOneTimePasswordOtherUserError(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice, bob := tu.UniqueString("alice"), tu.UniqueString("bob")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)
	_, err := aliceClient.GetOneTimePassword(aliceClient.Ctx(),
		&auth.GetOneTimePasswordRequest{
			Subject: bob,
		})
	require.YesError(t, err)
	require.Matches(t, "GetOneTimePassword", err.Error())
}

func TestOneTimePasswordExpires(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	var authCodeTTL int64 = 10 // seconds
	alice := tu.UniqueString("alice")
	aliceClient, anonClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, "")
	otpResp, err := aliceClient.GetOneTimePassword(aliceClient.Ctx(),
		&auth.GetOneTimePasswordRequest{
			TTL: authCodeTTL,
		})
	require.NoError(t, err)

	time.Sleep(time.Duration(authCodeTTL+1) * time.Second)
	authResp, err := anonClient.Authenticate(anonClient.Ctx(), &auth.AuthenticateRequest{
		OneTimePassword: otpResp.Code,
	})
	require.YesError(t, err)
	require.Nil(t, authResp)
}

// TestOTPTimeoutShorterThanSessionTimeout tests that GetOneTimePassword
// returns an OTP that cannot live longer than the session of the user
// that created it (in other words, you cannot extend your session by
// requesting and OTP and using it--only by re-authenticating or having an
// admin generate a token for you
func TestOTPTimeoutShorterThanSessionTimeout(t *testing.T) {
	// TODO(msteffen) test not written yet
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// Change aliceClient to use a short-lived token
	var tokenLifetime int64 = 15 // seconds (must be >10 per check in api_server)
	resp, err := aliceClient.GetAuthToken(aliceClient.Ctx(), &auth.GetAuthTokenRequest{
		TTL: tokenLifetime,
	})
	require.NoError(t, err)
	token1 := resp.Token
	aliceClient.SetAuthToken(token1)

	// Get a one-time password using the short-lived token
	otpResp, err := aliceClient.GetOneTimePassword(aliceClient.Ctx(),
		&auth.GetOneTimePasswordRequest{})
	require.NoError(t, err)
	authResp, err := aliceClient.Authenticate(aliceClient.Ctx(), &auth.AuthenticateRequest{
		OneTimePassword: otpResp.Code,
	})
	require.NoError(t, err)
	token2 := authResp.PachToken
	require.NotEqual(t, token1, token2) // OTP-based token is new
	aliceClient.SetAuthToken(token2)

	// The new token (from the OTP) works initially...
	who, err := aliceClient.WhoAmI(aliceClient.Ctx(), &auth.WhoAmIRequest{})
	require.NoError(t, err)
	require.Equal(t, gh(alice), who.Username)

	// ...but stops working after the original token expires
	time.Sleep(time.Duration(tokenLifetime+1) * time.Second)
	_, err = aliceClient.WhoAmI(aliceClient.Ctx(), &auth.WhoAmIRequest{})
	require.YesError(t, err)
	require.True(t, auth.IsErrBadToken(err), err.Error())
}

func TestS3GatewayAuthRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}

	// generate auth credentials
	aliceClient := tu.GetAuthenticatedPachClient(t, tu.UniqueString("alice"))
	authResp, err := aliceClient.GetAuthToken(aliceClient.Ctx(), &auth.GetAuthTokenRequest{})
	require.NoError(t, err)
	authToken := authResp.Token

	// anon login via V2 - should fail
	minioClientV2, err := minio.NewV2("127.0.0.1:30600", "", "", false)
	require.NoError(t, err)
	_, err = minioClientV2.ListBuckets()
	require.YesError(t, err)

	// anon login via V4 - should fail
	minioClientV4, err := minio.NewV4("127.0.0.1:30600", "", "", false)
	require.NoError(t, err)
	_, err = minioClientV4.ListBuckets()
	require.YesError(t, err)

	// proper login via V2 - should succeed
	minioClientV2, err = minio.NewV2("127.0.0.1:30600", authToken, authToken, false)
	require.NoError(t, err)
	_, err = minioClientV2.ListBuckets()
	require.NoError(t, err)

	// proper login via V4 - should succeed
	minioClientV2, err = minio.NewV4("127.0.0.1:30600", authToken, authToken, false)
	require.NoError(t, err)
	_, err = minioClientV2.ListBuckets()
	require.NoError(t, err)
}

// TestDeleteFailedPipeline creates a pipeline with an invalid image and then
// tries to delete it (which shouldn't be blocked by the auth system)
func TestDeleteFailedPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// Create input repo w/ initial commit
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	_, err := aliceClient.PutFile(repo, "master", "/file", strings.NewReader("1"))
	require.NoError(t, err)

	// Create pipeline
	pipeline := tu.UniqueString("pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"does-not-exist", // nonexistant image
		[]string{"true"}, nil,
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))
	require.NoError(t, aliceClient.DeletePipeline(pipeline, true))

	// make sure FlushCommit eventually returns (i.e. pipeline failure doesn't
	// block flushCommit indefinitely)
	iter, err := aliceClient.FlushCommit(
		[]*pfs.Commit{client.NewCommit(repo, "master")},
		[]*pfs.Repo{client.NewRepo(pipeline)})
	require.NoError(t, err)
	require.NoErrorWithinT(t, 30*time.Second, func() error {
		_, err := iter.Next()
		if !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	})
}

// TestDeletePipelineMissingRepos creates a pipeline, force-deletes its input
// and output repos, and then confirms that DeletePipeline still works (i.e.
// the missing repos/ACLs don't cause an auth error).
func TestDeletePipelineMissingRepos(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)
	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	// Create input repo w/ initial commit
	repo := tu.UniqueString(t.Name())
	require.NoError(t, aliceClient.CreateRepo(repo))
	_, err := aliceClient.PutFile(repo, "master", "/file", strings.NewReader("1"))
	require.NoError(t, err)

	// Create pipeline
	pipeline := tu.UniqueString("pipeline")
	require.NoError(t, aliceClient.CreatePipeline(
		pipeline,
		"does-not-exist", // nonexistant image
		[]string{"true"}, nil,
		&pps.ParallelismSpec{Constant: 1},
		client.NewPFSInput(repo, "/*"),
		"", // default output branch: master
		false,
	))

	// force-delete input and output repos
	require.NoError(t, aliceClient.DeleteRepo(repo, true))
	require.NoError(t, aliceClient.DeleteRepo(pipeline, true))

	// Attempt to delete the pipeline--must succeed
	require.NoError(t, aliceClient.DeletePipeline(pipeline, true))
	require.NoErrorWithinTRetry(t, 30*time.Second, func() error {
		pis, err := aliceClient.ListPipeline()
		if err != nil {
			return err
		}
		for _, pi := range pis {
			if pi.Pipeline.Name == pipeline {
				return errors.Errorf("Expected %q to be deleted, but still present", pipeline)
			}
		}
		return nil
	})
}

func TestDisableGitHubAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	// activate auth with initial admin robot:hub
	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// confirm config is set to default config
	cfg, err := adminClient.GetConfiguration(adminClient.Ctx(), &auth.GetConfigurationRequest{})
	require.NoError(t, err)
	requireConfigsEqual(t, &authserver.DefaultAuthConfig, cfg.GetConfiguration())

	// confirm GH auth works by default
	_, err = adminClient.Authenticate(adminClient.Ctx(), &auth.AuthenticateRequest{
		GitHubToken: "alice",
	})
	require.NoError(t, err)

	// set config to no GH, confirm it gets set
	configNoGitHub := &auth.AuthConfig{
		LiveConfigVersion: 1,
	}
	_, err = adminClient.SetConfiguration(adminClient.Ctx(), &auth.SetConfigurationRequest{
		Configuration: configNoGitHub,
	})
	require.NoError(t, err)

	cfg, err = adminClient.GetConfiguration(adminClient.Ctx(), &auth.GetConfigurationRequest{})
	require.NoError(t, err)
	configNoGitHub.LiveConfigVersion = 2
	requireConfigsEqual(t, configNoGitHub, cfg.GetConfiguration())

	// confirm GH auth doesn't work
	_, err = adminClient.Authenticate(adminClient.Ctx(), &auth.AuthenticateRequest{
		GitHubToken: "bob",
	})
	require.YesError(t, err)
	require.Equal(t, "rpc error: code = Unknown desc = GitHub auth is not enabled on this cluster", err.Error())

	// set conifg to allow GH auth again
	newerDefaultAuth := authserver.DefaultAuthConfig
	newerDefaultAuth.LiveConfigVersion = 2
	_, err = adminClient.SetConfiguration(adminClient.Ctx(), &auth.SetConfigurationRequest{
		Configuration: &newerDefaultAuth,
	})
	require.NoError(t, err)
	cfg, err = adminClient.GetConfiguration(adminClient.Ctx(), &auth.GetConfigurationRequest{})
	require.NoError(t, err)
	newerDefaultAuth.LiveConfigVersion = 3
	requireConfigsEqual(t, &newerDefaultAuth, cfg.GetConfiguration())

	// confirm GH works again
	_, err = adminClient.Authenticate(adminClient.Ctx(), &auth.AuthenticateRequest{
		GitHubToken: "carol",
	})
	require.NoError(t, err)

	// clean up
}

// TestDisableGitHubAuthFSAdmin tests that users with the FS admin role can't disable auth
func TestDisableGitHubAuthFSAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	aliceClient, adminClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// confirm config is set to default config
	cfg, err := adminClient.GetConfiguration(adminClient.Ctx(), &auth.GetConfigurationRequest{})
	require.NoError(t, err)
	requireConfigsEqual(t, &authserver.DefaultAuthConfig, cfg.GetConfiguration())

	// confirm GH auth works by default
	_, err = adminClient.Authenticate(adminClient.Ctx(), &auth.AuthenticateRequest{
		GitHubToken: "alice",
	})
	require.NoError(t, err)

	// admin makes alice an fs admin
	_, err = adminClient.ModifyClusterRoleBinding(adminClient.Ctx(),
		&auth.ModifyClusterRoleBindingRequest{Principal: gh(alice), Roles: fsClusterRole()})
	require.NoError(t, err)

	// wait until alice shows up in admin list
	require.NoError(t, backoff.Retry(func() error {
		resp, err := aliceClient.GetClusterRoleBindings(aliceClient.Ctx(), &auth.GetClusterRoleBindingsRequest{})
		require.NoError(t, err)
		return require.EqualOrErr(
			admins(tu.AdminUser)(gh(alice)), resp.Bindings,
		)
	}, backoff.NewTestingBackOff()))

	// alice tries to set config to no GH, but doesn't have permission
	configNoGitHub := &auth.AuthConfig{
		LiveConfigVersion: 1,
	}
	_, err = aliceClient.SetConfiguration(aliceClient.Ctx(), &auth.SetConfigurationRequest{
		Configuration: configNoGitHub,
	})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
}

// TestDeactivateFSAdmin tests that users with the FS admin role can't call Deactivate
func TestDeactivateFSAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	alice := tu.UniqueString("alice")
	aliceClient, adminClient := tu.GetAuthenticatedPachClient(t, alice), tu.GetAuthenticatedPachClient(t, tu.AdminUser)

	// admin makes alice an fs admin
	_, err := adminClient.ModifyClusterRoleBinding(adminClient.Ctx(),
		&auth.ModifyClusterRoleBindingRequest{Principal: gh(alice), Roles: fsClusterRole()})
	require.NoError(t, err)

	// wait until alice shows up in admin list
	require.NoError(t, backoff.Retry(func() error {
		resp, err := aliceClient.GetClusterRoleBindings(aliceClient.Ctx(), &auth.GetClusterRoleBindingsRequest{})
		require.NoError(t, err)
		return require.EqualOrErr(
			admins(tu.AdminUser)(gh(alice)), resp.Bindings,
		)
	}, backoff.NewTestingBackOff()))

	// alice tries to deactivate, but doesn't have permission as an FS admin
	_, err = aliceClient.Deactivate(aliceClient.Ctx(), &auth.DeactivateRequest{})
	require.YesError(t, err)
	require.Matches(t, "not authorized", err.Error())
}

// TODO: This test mirrors TestDebug in src/server/pachyderm_test.go.
// Need to restructure testing such that we have the implementation of this
// test in one place while still being able to test auth enabled and disabled clusters.
func TestDebug(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}
	tu.DeleteAll(t)
	defer tu.DeleteAll(t)

	alice := tu.UniqueString("alice")
	aliceClient := tu.GetAuthenticatedPachClient(t, alice)

	dataRepo := tu.UniqueString("TestDebug_data")
	require.NoError(t, aliceClient.CreateRepo(dataRepo))

	expectedFiles := make(map[string]*globlib.Glob)
	// Record glob patterns for expected pachd files.
	for _, file := range []string{"version", "logs", "goroutine", "heap"} {
		pattern := path.Join("pachd", "*", "pachd", file)
		g, err := globlib.Compile(pattern, '/')
		require.NoError(t, err)
		expectedFiles[pattern] = g
	}
	for i := 0; i < 3; i++ {
		pipeline := tu.UniqueString("TestDebug")
		require.NoError(t, aliceClient.CreatePipeline(
			pipeline,
			"",
			[]string{"bash"},
			[]string{
				fmt.Sprintf("cp /pfs/%s/* /pfs/out/", dataRepo),
			},
			&pps.ParallelismSpec{
				Constant: 1,
			},
			client.NewPFSInput(dataRepo, "/*"),
			"",
			false,
		))
		// Record glob patterns for expected pipeline files.
		for _, container := range []string{"user", "storage"} {
			for _, file := range []string{"logs", "goroutine", "heap"} {
				pattern := path.Join("pipelines", pipeline, "pods", "*", container, file)
				g, err := globlib.Compile(pattern, '/')
				require.NoError(t, err)
				expectedFiles[pattern] = g
			}
		}
		pattern := path.Join("pipelines", pipeline, "spec")
		g, err := globlib.Compile(pattern, '/')
		require.NoError(t, err)
		expectedFiles[pattern] = g
	}

	commit1, err := aliceClient.StartCommit(dataRepo, "master")
	require.NoError(t, err)
	_, err = aliceClient.PutFile(dataRepo, commit1.ID, "file", strings.NewReader("foo"))
	require.NoError(t, err)
	require.NoError(t, aliceClient.FinishCommit(dataRepo, commit1.ID))

	commitIter, err := aliceClient.FlushCommit([]*pfs.Commit{commit1}, nil)
	require.NoError(t, err)
	commitInfos := collectCommitInfos(t, commitIter)
	require.Equal(t, 3, len(commitInfos))

	// Only admins can collect a debug dump.
	buf := &bytes.Buffer{}
	require.YesError(t, aliceClient.Dump(nil, buf))
	adminClient := tu.GetAuthenticatedPachClient(t, tu.AdminUser)
	require.NoError(t, adminClient.Dump(nil, buf))
	gr, err := gzip.NewReader(buf)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, gr.Close())
	}()
	// Check that all of the expected files were returned.
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
		}
		for pattern, g := range expectedFiles {
			if g.Match(hdr.Name) {
				delete(expectedFiles, pattern)
				break
			}
		}
	}
	require.Equal(t, 0, len(expectedFiles))
}

func collectCommitInfos(t testing.TB, commitInfoIter client.CommitInfoIterator) []*pfs.CommitInfo {
	var commitInfos []*pfs.CommitInfo
	for {
		commitInfo, err := commitInfoIter.Next()
		if errors.Is(err, io.EOF) {
			return commitInfos
		}
		require.NoError(t, err)
		commitInfos = append(commitInfos, commitInfo)
	}
}
