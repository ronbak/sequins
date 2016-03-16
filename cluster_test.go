package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/samuel/go-zookeeper/zk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const expectTimeout = 5 * time.Second

type testVersion string

func randomPort() int {
	rand.Seed(time.Now().UnixNano())
	return int(rand.Int31n(6000) + 16000)
}

const dbName = "db"
const (
	start     testVersion = "START"
	noVersion testVersion = "NONE"
	v1        testVersion = "v1"
	v2        testVersion = "v2"
	v3        testVersion = "v3"
	down      testVersion = "DOWN"
	timeout   testVersion = "TIMEOUT"
)

type testCluster struct {
	*testing.T
	binary     string
	root       string
	sequinses  []*testSequins
	zkCluster  *zk.TestCluster
	zkServers  []string
	testClient *http.Client
}

type testSequins struct {
	*testing.T
	name                string
	binary              string
	storePath           string
	configPath          string
	backendPath         string
	config              sequinsConfig
	testClient          *http.Client
	expectedProgression []testVersion

	process     *exec.Cmd
	progression chan testVersion
}

func newTestCluster(t *testing.T) *testCluster {
	binary, _ := filepath.Abs("sequins")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("Skipping functional cluster tests because no binary is available. Please run the tests with 'make test'.")
	}

	root, err := ioutil.TempDir("", "sequins-cluster-")
	require.NoError(t, err)

	zkServers, zkCluster := createTestZkCluster(t)

	// We have a specific transport to the client, so it doesn't try to reuse
	// connections between tests
	var testClient = &http.Client{
		Timeout:   250 * time.Millisecond,
		Transport: &http.Transport{},
	}

	return &testCluster{
		T:          t,
		binary:     binary,
		root:       root,
		sequinses:  make([]*testSequins, 0),
		zkServers:  zkServers,
		zkCluster:  zkCluster,
		testClient: testClient,
	}
}

func (tc *testCluster) addSequins() *testSequins {
	port := randomPort()
	path := filepath.Join(tc.root, fmt.Sprintf("node-%d", port))

	storePath := filepath.Join(path, "store")
	err := os.MkdirAll(storePath, 0755|os.ModeDir)
	require.NoError(tc.T, err, "setup: create store path")

	backendPath := filepath.Join(path, "backend")
	err = os.MkdirAll(backendPath, 0755|os.ModeDir)
	require.NoError(tc.T, err, "setup: create backend path")

	configPath := filepath.Join(path, "sequins.conf")

	config := defaultConfig()
	name := fmt.Sprintf("localhost:%d", port)
	config.Bind = name
	config.Root = backendPath
	config.LocalStore = path
	config.RequireSuccessFile = true
	config.ThrottleLoads = duration{3 * time.Millisecond}
	config.ZK.Servers = tc.zkServers
	config.ZK.TimeToConverge = duration{100 * time.Millisecond}
	config.ZK.ProxyTimeout = duration{150 * time.Millisecond}
	config.ZK.AdvertisedHostname = "localhost"

	s := &testSequins{
		T:           tc.T,
		name:        name,
		binary:      tc.binary,
		backendPath: backendPath,
		configPath:  configPath,
		config:      config,
		testClient:  tc.testClient,

		progression: make(chan testVersion, 1024),
	}

	tc.sequinses = append(tc.sequinses, s)
	return s
}

func (tc *testCluster) addSequinses(n int) {
	for i := 0; i < n; i++ {
		tc.addSequins()
	}
}

func (tc *testCluster) expectProgression(versions ...testVersion) {
	for _, ts := range tc.sequinses {
		ts.expectProgression(versions...)
	}
}

func (tc *testCluster) setup() {
	for _, ts := range tc.sequinses {
		ts.setup()
	}
}

func (tc *testCluster) makeVersionAvailable(version testVersion) {
	for _, ts := range tc.sequinses {
		ts.makeVersionAvailable(version)
	}
}

func (tc *testCluster) removeAvailableVersion(version testVersion) {
	for _, ts := range tc.sequinses {
		ts.removeAvailableVersion(version)
	}
}

func (tc *testCluster) startTest() {
	for _, ts := range tc.sequinses {
		ts.startTest()
	}
}

func (tc *testCluster) assertProgression() {
	for _, ts := range tc.sequinses {
		ts.assertProgression()
	}
}

func (tc *testCluster) hup() {
	for _, ts := range tc.sequinses {
		ts.hup()
	}
}

func (tc *testCluster) tearDown() {
	for _, ts := range tc.sequinses {
		ts.process.Process.Kill()
	}

	tc.zkCluster.Stop()
	os.RemoveAll(tc.root)
}

func (ts *testSequins) expectProgression(versions ...testVersion) {
	ts.expectedProgression = versions
}

func (ts *testSequins) setup() {
	f, err := os.Create(ts.configPath)
	require.NoError(ts.T, err, "setup: create config")

	err = toml.NewEncoder(f).Encode(ts.config)
	require.NoError(ts.T, err, "setup: create config")
	f.Close()

	// TODO: if there is an init version, start sequins up, wait for it to index,
	// and then kill it.
}

func (ts *testSequins) makeVersionAvailable(version testVersion) {
	path := filepath.Join(ts.backendPath, dbName, string(version))
	err := directoryCopy(ts.T, path, "test/baby-names/1")
	require.NoError(ts.T, err, "setup: make version available: %s", version)

	f, err := os.Create(filepath.Join(path, "_SUCCESS"))
	require.NoError(ts.T, err, "setup: make version available: %s", version)

	f.Close()
}

func (ts *testSequins) removeAvailableVersion(version testVersion) {
	path := filepath.Join(ts.backendPath, dbName, string(version))
	os.RemoveAll(path)
}

func (ts *testSequins) startTest() {
	versions := make(chan testVersion)

	go func() {
		lastVersion := start
		for {
			now := time.Now()
			key := babyNames[rand.Intn(len(babyNames))].key
			url := fmt.Sprintf("http://%s/%s/%s", ts.name, dbName, key)

			var version testVersion
			resp, err := ts.testClient.Get(url)
			if err == nil {
				v := resp.Header.Get("X-Sequins-Version")
				resp.Body.Close()

				if resp.StatusCode > 404 {
					version = down
				} else if v == "" {
					version = noVersion
				} else {
					version = testVersion(v)
				}
			} else {
				// A number of timeouts are ok - this isn't the friendliest environment,
				// after all. We want to fail fast and frequently so that we don't
				// miss changes to the available version.
				netErr, ok := err.(net.Error)
				if ok && netErr.Timeout() {
					version = lastVersion
				} else {
					if lastVersion != down && lastVersion != start {
						ts.T.Logf("%s (lastVersion: %s)", err, lastVersion)
					}

					version = down
				}
			}

			if version != lastVersion {
				versions <- version
				lastVersion = version
			}

			// Sleep for 250 milliseconds less the time we took to make the last
			// request, such that we make a request roughly every 250 milliseconds.
			time.Sleep((250 * time.Millisecond) - time.Now().Sub(now))
		}
	}()

	if ts.process == nil {
		go func() {
			// Wait for the process to register as down, then start it.
			first := <-versions
			require.Equal(ts.T, down, first, "setup: sequins process should start as down")

			ts.start()
			ts.progression <- first
			for v := range versions {
				ts.progression <- v
			}
		}()
	}
}

func (ts *testSequins) start() {
	ts.process = exec.Command(ts.binary, "--config", ts.configPath)
	stdout, err := ts.process.StdoutPipe()
	require.NoError(ts.T, err, "setup: hooking into process stdout")

	stderr, err := ts.process.StderrPipe()
	require.NoError(ts.T, err, "setup: hooking into process stderr")

	go func() {
		stdoutScanner := bufio.NewScanner(stdout)
		for stdoutScanner.Scan() {
			ts.T.Logf("[stdout %s] %s", ts.name, stdoutScanner.Text())
		}

		stderrScanner := bufio.NewScanner(stderr)
		for stderrScanner.Scan() {
			ts.T.Logf("[stderr %s] %s", ts.name, stderrScanner.Text())
		}
	}()

	ts.process.Start()
}

func (ts *testSequins) hup() {
	ts.process.Process.Signal(syscall.SIGHUP)
}

func (ts *testSequins) assertProgression() {
	var actualProgression []testVersion

Progression:
	for range ts.expectedProgression {
		t := time.NewTimer(expectTimeout)

		select {
		case v := <-ts.progression:
			actualProgression = append(actualProgression, v)
		case <-t.C:
			actualProgression = append(actualProgression, timeout)
			break Progression
		}
	}

	expected := ""
	for _, v := range ts.expectedProgression {
		if expected != "" {
			expected += " -> "
		}

		expected += string(v)
	}

	actual := ""
	for _, v := range actualProgression {
		if actual != "" {
			actual += " -> "
		}

		actual += string(v)
	}

	assert.Equal(ts.T, expected, actual, "unexpected progression for %s", ts.name)
}

// TestClusterEmptySingleNode tests that a node with no preexisting state can start up
// and serve requests.
func TestClusterEmptySingleNode(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(1)
	tc.makeVersionAvailable(v3)
	tc.expectProgression(down, noVersion, v3)

	tc.setup()
	tc.startTest()
	tc.assertProgression()
}

// TestClusterUpgradingSingleNode tests that a node can upgrade to one version, and
// then upgrade a second and third time.
func TestClusterUpgradingSingleNode(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(1)
	tc.makeVersionAvailable(v1)
	tc.expectProgression(down, noVersion, v1, v2, v3)

	tc.setup()
	tc.startTest()

	time.Sleep(expectTimeout)
	tc.makeVersionAvailable(v2)
	tc.hup()

	time.Sleep(expectTimeout)
	tc.makeVersionAvailable(v3)
	tc.hup()

	tc.assertProgression()
}

// TestClusterEmpty tests that a cluster with no preexisting state can start up
// and serve requests.
func TestClusterEmpty(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(3)
	tc.makeVersionAvailable(v3)
	tc.expectProgression(down, noVersion, v3)

	tc.setup()
	tc.startTest()
	tc.assertProgression()
}

// TestClusterUpgrading tests that a node can upgrade to one version, and
// then upgrade a second and third time.
func TestClusterUpgrading(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(3)
	tc.makeVersionAvailable(v1)
	tc.expectProgression(down, noVersion, v1, v2, v3)

	tc.setup()
	tc.startTest()

	time.Sleep(expectTimeout)
	tc.makeVersionAvailable(v2)
	tc.hup()

	time.Sleep(expectTimeout)
	tc.makeVersionAvailable(v3)
	tc.hup()

	tc.assertProgression()
}

// TestClusterDelayedUpgrade tests that one node can upgrade several seconds earlier
// that the rest of the cluster without losing any reads.
func TestClusterDelayedUpgrade(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(3)
	tc.expectProgression(down, noVersion, v1, v2)

	tc.makeVersionAvailable(v1)
	tc.setup()
	tc.startTest()

	time.Sleep(expectTimeout)
	tc.makeVersionAvailable(v2)
	tc.sequinses[0].hup()

	time.Sleep(expectTimeout)
	tc.hup()

	tc.assertProgression()
}

// TestClusterNoDowngrade tests that a cluster will never downgrade to an older
// version, even if the newer one is available.
func TestClusterNoDowngrade(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(3)
	tc.expectProgression(down, noVersion, v3)

	tc.makeVersionAvailable(v3)
	tc.setup()
	tc.startTest()

	time.Sleep(expectTimeout)
	tc.makeVersionAvailable(v2)
	tc.removeAvailableVersion(v3)
	tc.hup()

	tc.assertProgression()
}

// TestClusterLateJoin tests if a node can join an existing cluster and
// immediately start serving the version that the rest of the cluster has.
func TestClusterLateJoin(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(3)
	tc.expectProgression(down, noVersion, v3)

	tc.makeVersionAvailable(v3)
	tc.setup()
	tc.startTest()
	time.Sleep(expectTimeout)

	s := tc.addSequins()
	s.makeVersionAvailable(v3)
	s.setup()
	s.expectProgression(down, v3)
	s.startTest()

	tc.assertProgression()
}

// TestClusterNodeWithoutData tests if a node can safely stay behind while
// the rest of the cluster upgrades.
func TestClusterNodeWithoutData(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cluster test in short mode.")
	}

	tc := newTestCluster(t)
	defer tc.tearDown()

	tc.addSequinses(3)
	tc.sequinses[0].expectProgression(down, noVersion, v1)
	tc.sequinses[1].expectProgression(down, noVersion, v1, v2, v3)
	tc.sequinses[2].expectProgression(down, noVersion, v1, v2, v3)

	tc.makeVersionAvailable(v1)
	tc.setup()
	tc.startTest()

	time.Sleep(expectTimeout)
	tc.sequinses[1].makeVersionAvailable(v2)
	tc.sequinses[2].makeVersionAvailable(v2)
	tc.hup()

	time.Sleep(expectTimeout)
	tc.sequinses[1].makeVersionAvailable(v3)
	tc.sequinses[2].makeVersionAvailable(v3)
	tc.hup()

	tc.assertProgression()
}