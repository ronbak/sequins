package main

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	zk "launchpad.net/gozk/zookeeper"
)

func randomPort() int {
	rand.Seed(time.Now().UnixNano())
	return int(rand.Int31n(6000) + 16000)
}

type testZK struct {
	*testing.T
	home string
	dir  string
	port int
	addr string
	zk   *zk.Server
}

func (tzk *testZK) start() {
	err := tzk.zk.Start()
	require.NoError(tzk.T, err, "zk start")
	time.Sleep(time.Second)
}

func (tzk *testZK) close() {
	log, err := ioutil.TempFile("", "sequins-test-zookeeper-")
	require.NoError(tzk.T, err, "setup: copying log")
	log.Close()

	err = os.Rename(filepath.Join(tzk.dir, "log.txt"), log.Name())
	require.NoError(tzk.T, err, "setup: copying log")

	tzk.T.Logf("Zookeeper output at %s", log.Name())
	tzk.zk.Destroy()
}

func (tzk *testZK) restart() {
	tzk.zk.Stop()
	time.Sleep(time.Second)
	tzk.start()
}

func createTestZk(t *testing.T) *testZK {
	zkHome := os.Getenv("ZOOKEEPER_HOME")
	if zkHome == "" {
		t.Skip("Skipping zk tests because ZOOKEEPER_HOME isn't set")
	}

	dir, err := ioutil.TempDir("", "sequins-zk")
	require.NoError(t, err, "zk setup")

	port := randomPort()
	zk, err := zk.CreateServer(port, dir, zkHome)
	require.NoError(t, err, "zk setup")

	tzk := testZK{
		T:    t,
		home: zkHome,
		dir:  dir,
		port: port,
		addr: fmt.Sprintf("127.0.0.1:%d", port),
		zk:   zk,
	}

	tzk.start()
	return &tzk
}

func connectZookeeperTest(t *testing.T) (*zkWatcher, *testZK) {
	tzk := createTestZk(t)

	zkWatcher, err := connectZookeeper([]string{tzk.addr}, "/sequins-test", 5*time.Second, 5*time.Second)
	require.NoError(t, err, "zkWatcher should connect")

	return zkWatcher, tzk
}

func expectWatchUpdate(t *testing.T, expected []string, updates chan []string, msg string) {
	sort.Strings(expected)
	timer := time.NewTimer(20 * time.Second)
	select {
	case update := <-updates:
		sort.Strings(update)
		assert.Equal(t, expected, update, msg)
	case <-timer.C:
		require.FailNow(t, "timed out waiting for update")
	}
}

func TestZKWatcher(t *testing.T) {
	w, tzk := connectZookeeperTest(t)
	defer w.close()
	defer tzk.close()

	updates, _ := w.watchChildren("/foo")
	go func() {
		w.createEphemeral("/foo/bar")
		time.Sleep(100 * time.Millisecond)
		w.removeEphemeral("/foo/bar")
	}()

	expectWatchUpdate(t, nil, updates, "the list of children should be updated to be empty first")
	expectWatchUpdate(t, []string{"bar"}, updates, "the list of children should be updated with the new node")
	expectWatchUpdate(t, nil, updates, "the list of children should be updated to be empty again")
}

func TestZKWatcherReconnect(t *testing.T) {
	w, tzk := connectZookeeperTest(t)
	defer w.close()
	defer tzk.close()

	updates, _ := w.watchChildren("/foo")
	go func() {
		w.createEphemeral("/foo/bar")
		time.Sleep(100 * time.Millisecond)
		tzk.restart()
		w.createEphemeral("/foo/baz")
	}()

	expectWatchUpdate(t, nil, updates, "the list of children should be updated to be empty first")
	expectWatchUpdate(t, []string{"bar"}, updates, "the list of children should be updated with the new node")
	expectWatchUpdate(t, []string{"bar", "baz"}, updates, "the list of children should be updated with the second new node")
}

func TestZKWatchesCanceled(t *testing.T) {
	w, tzk := connectZookeeperTest(t)
	defer w.close()
	defer tzk.close()

	w.watchChildren("/foo")

	for i := 0; i < 3; i++ {
		tzk.restart()
	}

	assert.Equal(t, 1, zk.CountPendingWatches(), "there should only be a single watch open")
}

func TestZKRemoveWatch(t *testing.T) {
	w, tzk := connectZookeeperTest(t)
	defer w.close()
	defer tzk.close()

	updates, disconnected := w.watchChildren("/foo")

	w.createEphemeral("/foo/bar")
	expectWatchUpdate(t, nil, updates, "the list of children should be updated to be empty first")
	expectWatchUpdate(t, []string{"bar"}, updates, "the list of children should be updated with the new node")

	w.removeWatch("/foo")

	// This is a sketchy way to make sure the updates channel gets closed.
	closed := make(chan bool)
	go func() {
		for range updates {
		}
		closed <- true
	}()

	timer := time.NewTimer(100 * time.Millisecond)
	select {
	case <-closed:
	case <-timer.C:
		assert.Fail(t, "the updates channel should be closed")
	}

	// And again for disconnected. This can't be a method, since updates and
	// disconnected don't have the same type.
	go func() {
		for range disconnected {
		}
		closed <- true
	}()

	timer.Reset(100 * time.Millisecond)
	select {
	case <-closed:
	case <-timer.C:
		assert.Fail(t, "the disconnected channel should be closed")
	}
}
