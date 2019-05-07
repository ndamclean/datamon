// +build fuse_cli

package cmd

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBundleMount(t *testing.T) {
	cleanup := setupTests(t)
	defer cleanup()
	runCmd(t, []string{"repo",
		"create",
		"--description", "testing",
		"--repo", repo1,
		"--name", "tests",
		"--email", "datamon@oneconcern.com",
	}, "create repo", false)
	runCmd(t, []string{"bundle",
		"upload",
		"--path", dirPathStr(t, testUploadTrees[1][0]),
		"--message", "read-only mount test bundle",
		"--repo", repo1,
	}, "upload bundle in order to test downloading individual files", false)
	rll, err := listBundles(t, repo1)
	require.NoError(t, err, "error out of listBundles() test helper")
	require.Equal(t, 1, len(rll), "bundle count in test repo")
	const (
		pathBackingFs = "/tmp/mmfs"
		pathToMount   = "/tmp/mmp"
	)
	require.NoError(t, os.Mkdir(pathBackingFs, 0777|os.ModeDir))
	require.NoError(t, os.Mkdir(pathToMount, 0777|os.ModeDir))
	defer os.RemoveAll(pathBackingFs)
	defer os.RemoveAll(pathToMount)
	cmd := exec.Command(
		"../datamon",
		"bundle", "mount",
		"--repo", repo1,
		"--bundle", rll[0].hash,
		"--destination", pathBackingFs,
		"--mount", pathToMount,
		"--meta", repoParams.MetadataBucket,
		"--blob", repoParams.BlobBucket,
	)
	require.NoError(t, cmd.Start())
	time.Sleep(5 * time.Second)
	for _, file := range testUploadTrees[1] {
		expected := readTextFile(t, filePathStr(t, file))
		actual := readTextFile(t, filepath.Join(pathToMount, pathInBundle(file)))
		require.Equal(t, len(expected), len(actual), "downloaded file '"+pathInBundle(file)+"' size")
		require.Equal(t, expected, actual, "downloaded file '"+pathInBundle(file)+"' contents")
	}
	require.NoError(t, cmd.Process.Kill())
	err = cmd.Wait()
	require.Equal(t, "signal: killed", err.Error(), "cmd exit with killed error")
}

func mutableMountOutputToBundleID(t *testing.T, out string) string {
	lines := strings.Split(out, "\n")
	var bundleKVLine string
	if strings.TrimSpace(lines[len(lines)-1]) == "" {
		bundleKVLine = lines[len(lines)-2]
	} else {
		bundleKVLine = lines[len(lines)-1]
	}
	bundleKV := strings.Split(bundleKVLine, ":")
	require.Equal(t, "bundle", strings.TrimSpace(bundleKV[0]))
	return strings.TrimSpace(bundleKV[1])
}

func TestBundleMutableMount(t *testing.T) {
	cleanup := setupTests(t)
	defer cleanup()
	runCmd(t, []string{"repo",
		"create",
		"--description", "testing",
		"--repo", repo1,
		"--name", "tests",
		"--email", "datamon@oneconcern.com",
	}, "create repo", false)
	const (
		pathBackingFs = "/tmp/mmfs"
		pathToMount   = "/tmp/mmp"
	)
	require.NoError(t, os.Mkdir(pathBackingFs, 0777|os.ModeDir))
	require.NoError(t, os.Mkdir(pathToMount, 0777|os.ModeDir))
	defer os.RemoveAll(pathBackingFs)
	defer os.RemoveAll(pathToMount)
	rll, err := listBundles(t, repo1)
	require.NoError(t, err, "error out of listBundles() test helper")
	require.Equal(t, 0, len(rll), "bundle count in test repo")
	cmd := exec.Command(
		"../datamon",
		"bundle", "mount", "new",
		"--repo", repo1,
		"--message", "mutabletest",
		"--destination", pathBackingFs,
		"--mount", pathToMount,
		"--meta", repoParams.MetadataBucket,
		"--blob", repoParams.BlobBucket,
	)
	rdr, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	time.Sleep(1 * time.Second)
	createTestUploadTree(t, pathToMount, testUploadTrees[1])
	backingFileInfos, err := ioutil.ReadDir(pathBackingFs)
	require.NoError(t, err)
	require.Equal(t, len(testUploadTrees[1]), len(backingFileInfos),
		"found expected count of files stored by inode")
	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	bytes, err := ioutil.ReadAll(rdr)
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	mutableMountOutput := string(bytes)
	t.Logf("mutableMountOutput: %v", mutableMountOutput)
	bundleID := mutableMountOutputToBundleID(t, mutableMountOutput)
	rll, err = listBundles(t, repo1)
	require.NoError(t, err, "error out of listBundles() test helper")
	require.Equal(t, 1, len(rll), "bundle count in test repo")
	t.Logf("bundles list output %v", rll[0])
	require.Equal(t, bundleID, rll[0].hash)
}
