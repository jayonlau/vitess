package vreplication

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"vitess.io/vitess/go/vt/vtgate/planbuilder"

	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/vt/log"
)

var (
	debug = false // set to true for local debugging: this uses the local env vtdataroot and does not teardown clusters

	originalVtdataroot    string
	vtdataroot            string
	mainClusterConfig     *ClusterConfig
	externalClusterConfig *ClusterConfig
	extraVTGateArgs       = []string{"-tablet_refresh_interval", "10ms"}
)

// ClusterConfig defines the parameters like ports, tmpDir, tablet types which uniquely define a vitess cluster
type ClusterConfig struct {
	charset              string
	hostname             string
	topoPort             int
	vtctldPort           int
	vtctldGrpcPort       int
	vtdataroot           string
	tmpDir               string
	vtgatePort           int
	vtgateGrpcPort       int
	vtgateMySQLPort      int
	vtgatePlannerVersion planbuilder.PlannerVersion
	tabletTypes          string
	tabletPortBase       int
	tabletGrpcPortBase   int
	tabletMysqlPortBase  int

	vreplicationCompressGTID bool
}

// VitessCluster represents all components within the test cluster
type VitessCluster struct {
	ClusterConfig *ClusterConfig
	Name          string
	Cells         map[string]*Cell
	Topo          *cluster.TopoProcess
	Vtctld        *cluster.VtctldProcess
	Vtctl         *cluster.VtctlProcess
	VtctlClient   *cluster.VtctlClientProcess
}

// Cell represents a Vitess cell within the test cluster
type Cell struct {
	Name      string
	Keyspaces map[string]*Keyspace
	Vtgates   []*cluster.VtgateProcess
}

// Keyspace represents a Vitess keyspace contained by a cell within the test cluster
type Keyspace struct {
	Name    string
	Shards  map[string]*Shard
	VSchema string
	Schema  string
}

// Shard represents a Vitess shard in a keyspace
type Shard struct {
	Name      string
	IsSharded bool
	Tablets   map[string]*Tablet
}

// Tablet represents a vttablet within a shard
type Tablet struct {
	Name     string
	Vttablet *cluster.VttabletProcess
	DbServer *cluster.MysqlctlProcess
}

func setTempVtDataRoot() string {
	dirSuffix := 100000 + rand.Intn(999999-100000) // 6 digits
	if debug {
		vtdataroot = originalVtdataroot
	} else {
		vtdataroot = path.Join(originalVtdataroot, fmt.Sprintf("vreple2e_%d", dirSuffix))
	}
	if _, err := os.Stat(vtdataroot); os.IsNotExist(err) {
		os.Mkdir(vtdataroot, 0700)
	}
	_ = os.Setenv("VTDATAROOT", vtdataroot)
	fmt.Printf("VTDATAROOT is %s\n", vtdataroot)
	return vtdataroot
}

func getClusterConfig(idx int, dataRootDir string) *ClusterConfig {
	basePort := 15000
	etcdPort := 2379

	basePort += idx * 10000
	etcdPort += idx * 10000
	if _, err := os.Stat(dataRootDir); os.IsNotExist(err) {
		os.Mkdir(dataRootDir, 0700)
	}

	return &ClusterConfig{
		hostname:            "localhost",
		topoPort:            etcdPort,
		vtctldPort:          basePort,
		vtctldGrpcPort:      basePort + 999,
		tmpDir:              dataRootDir + "/tmp",
		vtgatePort:          basePort + 1,
		vtgateGrpcPort:      basePort + 991,
		vtgateMySQLPort:     basePort + 306,
		tabletTypes:         "primary",
		vtdataroot:          dataRootDir,
		tabletPortBase:      basePort + 1000,
		tabletGrpcPortBase:  basePort + 1991,
		tabletMysqlPortBase: basePort + 1306,
		charset:             "utf8mb4",
	}
}

func init() {
	// for local debugging set this variable so that each run uses VTDATAROOT instead of a random dir
	// and also does not teardown the cluster for inspecting logs and the databases
	if os.Getenv("VREPLICATION_E2E_DEBUG") != "" {
		debug = true
	}
	rand.Seed(time.Now().UTC().UnixNano())
	originalVtdataroot = os.Getenv("VTDATAROOT")
	var mainVtDataRoot string
	if debug {
		mainVtDataRoot = originalVtdataroot
	} else {
		mainVtDataRoot = setTempVtDataRoot()
	}
	mainClusterConfig = getClusterConfig(0, mainVtDataRoot)
	externalClusterConfig = getClusterConfig(1, mainVtDataRoot+"/ext")
}

// NewVitessCluster starts a basic cluster with vtgate, vtctld and the topo
func NewVitessCluster(t *testing.T, name string, cellNames []string, clusterConfig *ClusterConfig) *VitessCluster {
	vc := &VitessCluster{Name: name, Cells: make(map[string]*Cell), ClusterConfig: clusterConfig}
	require.NotNil(t, vc)
	topo := cluster.TopoProcessInstance(vc.ClusterConfig.topoPort, vc.ClusterConfig.topoPort+1, vc.ClusterConfig.hostname, "etcd2", "global")

	require.NotNil(t, topo)
	require.Nil(t, topo.Setup("etcd2", nil))
	topo.ManageTopoDir("mkdir", "/vitess/global")
	vc.Topo = topo
	for _, cellName := range cellNames {
		topo.ManageTopoDir("mkdir", "/vitess/"+cellName)
	}

	vtctld := cluster.VtctldProcessInstance(vc.ClusterConfig.vtctldPort, vc.ClusterConfig.vtctldGrpcPort,
		vc.ClusterConfig.topoPort, vc.ClusterConfig.hostname, vc.ClusterConfig.tmpDir)
	vc.Vtctld = vtctld
	require.NotNil(t, vc.Vtctld)
	// use first cell as `-cell`
	vc.Vtctld.Setup(cellNames[0])

	vc.Vtctl = cluster.VtctlProcessInstance(vc.ClusterConfig.topoPort, vc.ClusterConfig.hostname)
	require.NotNil(t, vc.Vtctl)
	for _, cellName := range cellNames {
		vc.Vtctl.AddCellInfo(cellName)
		cell, err := vc.AddCell(t, cellName)
		require.NoError(t, err)
		require.NotNil(t, cell)
	}

	vc.VtctlClient = cluster.VtctlClientProcessInstance(vc.ClusterConfig.hostname, vc.Vtctld.GrpcPort, vc.ClusterConfig.tmpDir)
	require.NotNil(t, vc.VtctlClient)

	return vc
}

// AddKeyspace creates a keyspace with specified shard keys and number of replica/read-only tablets
func (vc *VitessCluster) AddKeyspace(t *testing.T, cells []*Cell, ksName string, shards string, vschema string, schema string, numReplicas int, numRdonly int, tabletIDBase int) (*Keyspace, error) {
	keyspace := &Keyspace{
		Name:   ksName,
		Shards: make(map[string]*Shard),
	}

	if err := vc.Vtctl.CreateKeyspace(keyspace.Name); err != nil {
		t.Fatalf(err.Error())
	}
	cellsToWatch := ""
	for i, cell := range cells {
		if i > 0 {
			cellsToWatch = cellsToWatch + ","
		}
		cell.Keyspaces[ksName] = keyspace
		cellsToWatch = cellsToWatch + cell.Name
	}
	require.NoError(t, vc.AddShards(t, cells, keyspace, shards, numReplicas, numRdonly, tabletIDBase))

	if schema != "" {
		if err := vc.VtctlClient.ApplySchema(ksName, schema); err != nil {
			t.Fatalf(err.Error())
		}
	}
	keyspace.Schema = schema
	if vschema != "" {
		if err := vc.VtctlClient.ApplyVSchema(ksName, vschema); err != nil {
			t.Fatalf(err.Error())
		}
	}
	keyspace.VSchema = vschema
	for _, cell := range cells {
		if len(cell.Vtgates) == 0 {
			log.Infof("Starting vtgate")
			vc.StartVtgate(t, cell, cellsToWatch)
		}
	}
	_ = vc.VtctlClient.ExecuteCommand("RebuildKeyspaceGraph", ksName)
	return keyspace, nil
}

// AddTablet creates new tablet with specified attributes
func (vc *VitessCluster) AddTablet(t testing.TB, cell *Cell, keyspace *Keyspace, shard *Shard, tabletType string, tabletID int) (*Tablet, *exec.Cmd, error) {
	tablet := &Tablet{}

	options := []string{
		"-queryserver-config-schema-reload-time", "5",
		"-enable-lag-throttler",
		"-heartbeat_enable",
		"-heartbeat_interval", "250ms",
	} //FIXME: for multi-cell initial schema doesn't seem to load without "-queryserver-config-schema-reload-time"

	if mainClusterConfig.vreplicationCompressGTID {
		options = append(options, "-vreplication_store_compressed_gtid=true")
	}

	vttablet := cluster.VttabletProcessInstance(
		vc.ClusterConfig.tabletPortBase+tabletID,
		vc.ClusterConfig.tabletGrpcPortBase+tabletID,
		tabletID,
		cell.Name,
		shard.Name,
		keyspace.Name,
		vc.ClusterConfig.vtctldPort,
		tabletType,
		vc.Topo.Port,
		vc.ClusterConfig.hostname,
		vc.ClusterConfig.tmpDir,
		options,
		false,
		vc.ClusterConfig.charset)

	require.NotNil(t, vttablet)
	vttablet.SupportsBackup = false

	tablet.DbServer = cluster.MysqlCtlProcessInstance(tabletID, vc.ClusterConfig.tabletMysqlPortBase+tabletID, vc.ClusterConfig.tmpDir)
	require.NotNil(t, tablet.DbServer)
	tablet.DbServer.InitMysql = true
	proc, err := tablet.DbServer.StartProcess()
	if err != nil {
		t.Fatal(err.Error())
	}
	require.NotNil(t, proc)
	tablet.Name = fmt.Sprintf("%s-%d", cell.Name, tabletID)
	vttablet.Name = tablet.Name
	tablet.Vttablet = vttablet
	shard.Tablets[tablet.Name] = tablet

	return tablet, proc, nil
}

// AddShards creates shards given list of comma-separated keys with specified tablets in each shard
func (vc *VitessCluster) AddShards(t testing.TB, cells []*Cell, keyspace *Keyspace, names string, numReplicas int, numRdonly int, tabletIDBase int) error {
	arrNames := strings.Split(names, ",")
	log.Infof("Addshards got %d shards with %+v", len(arrNames), arrNames)
	isSharded := len(arrNames) > 1
	primaryTabletUID := 0
	for ind, shardName := range arrNames {
		tabletID := tabletIDBase + ind*100
		tabletIndex := 0
		shard := &Shard{Name: shardName, IsSharded: isSharded, Tablets: make(map[string]*Tablet, 1)}
		if _, ok := keyspace.Shards[shardName]; ok {
			log.Infof("Shard %s already exists, not adding", shardName)
		} else {
			log.Infof("Adding Shard %s", shardName)
			if err := vc.VtctlClient.ExecuteCommand("CreateShard", keyspace.Name+"/"+shardName); err != nil {
				t.Fatalf("CreateShard command failed with %+v\n", err)
			}
			keyspace.Shards[shardName] = shard
		}
		for i, cell := range cells {
			dbProcesses := make([]*exec.Cmd, 0)
			tablets := make([]*Tablet, 0)
			if i == 0 {
				// only add primary tablet for first cell, so first time CreateShard is called
				log.Infof("Adding Primary tablet")
				primary, proc, err := vc.AddTablet(t, cell, keyspace, shard, "replica", tabletID+tabletIndex)
				require.NoError(t, err)
				require.NotNil(t, primary)
				tabletIndex++
				primary.Vttablet.VreplicationTabletType = "PRIMARY"
				tablets = append(tablets, primary)
				dbProcesses = append(dbProcesses, proc)
				primaryTabletUID = primary.Vttablet.TabletUID
			}

			for i := 0; i < numReplicas; i++ {
				log.Infof("Adding Replica tablet")
				tablet, proc, err := vc.AddTablet(t, cell, keyspace, shard, "replica", tabletID+tabletIndex)
				require.NoError(t, err)
				require.NotNil(t, tablet)
				tabletIndex++
				tablets = append(tablets, tablet)
				dbProcesses = append(dbProcesses, proc)
			}
			for i := 0; i < numRdonly; i++ {
				log.Infof("Adding RdOnly tablet")
				tablet, proc, err := vc.AddTablet(t, cell, keyspace, shard, "rdonly", tabletID+tabletIndex)
				require.NoError(t, err)
				require.NotNil(t, tablet)
				tabletIndex++
				tablets = append(tablets, tablet)
				dbProcesses = append(dbProcesses, proc)
			}

			for ind, proc := range dbProcesses {
				log.Infof("Waiting for mysql process for tablet %s", tablets[ind].Name)
				if err := proc.Wait(); err != nil {
					t.Fatalf("%v :: Unable to start mysql server for %v", err, tablets[ind].Vttablet)
				}
			}
			for ind, tablet := range tablets {
				log.Infof("Creating vt_keyspace database for tablet %s", tablets[ind].Name)
				if _, err := tablet.Vttablet.QueryTablet(fmt.Sprintf("create database vt_%s", keyspace.Name),
					keyspace.Name, false); err != nil {
					t.Fatalf("Unable to start create database vt_%s for tablet %v", keyspace.Name, tablet.Vttablet)
				}
				log.Infof("Running Setup() for vttablet %s", tablets[ind].Name)
				if err := tablet.Vttablet.Setup(); err != nil {
					t.Fatalf(err.Error())
				}
			}
		}
		require.NotEqual(t, 0, primaryTabletUID, "Should have created a primary tablet")
		log.Infof("InitShardPrimary for %d", primaryTabletUID)
		require.NoError(t, vc.VtctlClient.InitShardPrimary(keyspace.Name, shardName, cells[0].Name, primaryTabletUID))
		log.Infof("Finished creating shard %s", shard.Name)
	}
	return nil
}

// DeleteShard deletes a shard
func (vc *VitessCluster) DeleteShard(t testing.TB, cellName string, ksName string, shardName string) {
	shard := vc.Cells[cellName].Keyspaces[ksName].Shards[shardName]
	require.NotNil(t, shard)
	for _, tab := range shard.Tablets {
		log.Infof("Shutting down tablet %s", tab.Name)
		tab.Vttablet.TearDown()
	}
	log.Infof("Deleting Shard %s", shardName)
	//TODO how can we avoid the use of even_if_serving?
	if output, err := vc.VtctlClient.ExecuteCommandWithOutput("DeleteShard", "-recursive", "-even_if_serving", ksName+"/"+shardName); err != nil {
		t.Fatalf("DeleteShard command failed with error %+v and output %s\n", err, output)
	}

}

// StartVtgate starts a vtgate process
func (vc *VitessCluster) StartVtgate(t testing.TB, cell *Cell, cellsToWatch string) {
	vtgate := cluster.VtgateProcessInstance(
		vc.ClusterConfig.vtgatePort,
		vc.ClusterConfig.vtgateGrpcPort,
		vc.ClusterConfig.vtgateMySQLPort,
		cell.Name,
		cellsToWatch,
		vc.ClusterConfig.hostname,
		vc.ClusterConfig.tabletTypes,
		vc.ClusterConfig.topoPort,
		vc.ClusterConfig.tmpDir,
		extraVTGateArgs,
		vc.ClusterConfig.vtgatePlannerVersion)
	require.NotNil(t, vtgate)
	if err := vtgate.Setup(); err != nil {
		t.Fatalf(err.Error())
	}
	cell.Vtgates = append(cell.Vtgates, vtgate)
}

// AddCell adds a new cell to the cluster
func (vc *VitessCluster) AddCell(t testing.TB, name string) (*Cell, error) {
	cell := &Cell{Name: name, Keyspaces: make(map[string]*Keyspace), Vtgates: make([]*cluster.VtgateProcess, 0)}
	vc.Cells[name] = cell
	return cell, nil
}

func (vc *VitessCluster) teardown(t testing.TB) {
	for _, cell := range vc.Cells {
		for _, vtgate := range cell.Vtgates {
			if err := vtgate.TearDown(); err != nil {
				log.Errorf("Error in vtgate teardown - %s", err.Error())
			} else {
				log.Infof("vtgate teardown successful")
			}
		}
	}
	//collect unique keyspaces across cells
	keyspaces := make(map[string]*Keyspace)
	for _, cell := range vc.Cells {
		for _, keyspace := range cell.Keyspaces {
			keyspaces[keyspace.Name] = keyspace
		}
	}

	var wg sync.WaitGroup

	for _, keyspace := range keyspaces {
		for _, shard := range keyspace.Shards {
			for _, tablet := range shard.Tablets {
				wg.Add(1)
				go func(tablet2 *Tablet) {
					defer wg.Done()
					if tablet2.DbServer != nil && tablet2.DbServer.TabletUID > 0 {
						if _, err := tablet2.DbServer.StopProcess(); err != nil {
							log.Infof("Error stopping mysql process: %s", err.Error())
						}
					}
					if err := tablet2.Vttablet.TearDown(); err != nil {
						log.Infof("Error stopping vttablet %s %s", tablet2.Name, err.Error())
					} else {
						log.Infof("Successfully stopped vttablet %s", tablet2.Name)
					}
				}(tablet)
			}
		}
	}
	wg.Wait()
	if err := vc.Vtctld.TearDown(); err != nil {
		log.Infof("Error stopping Vtctld:  %s", err.Error())
	} else {
		log.Info("Successfully stopped vtctld")
	}

	for _, cell := range vc.Cells {
		if err := vc.Topo.TearDown(cell.Name, originalVtdataroot, vtdataroot, false, "etcd2"); err != nil {
			log.Infof("Error in etcd teardown - %s", err.Error())
		} else {
			log.Infof("Successfully tore down topo %s", vc.Topo.Name)
		}
	}
}

// TearDown brings down a cluster, deleting processes, removing topo keys
func (vc *VitessCluster) TearDown(t testing.TB) {
	if debug {
		return
	}
	done := make(chan bool)
	go func() {
		vc.teardown(t)
		done <- true
	}()
	select {
	case <-done:
		log.Infof("TearDown() was successful")
	case <-time.After(1 * time.Minute):
		log.Infof("TearDown() timed out")
	}
	// some processes seem to hang around for a bit
	time.Sleep(5 * time.Second)
}

func (vc *VitessCluster) getVttabletsInKeyspace(t *testing.T, cell *Cell, ksName string, tabletType string) map[string]*cluster.VttabletProcess {
	keyspace := cell.Keyspaces[ksName]
	tablets := make(map[string]*cluster.VttabletProcess)
	for _, shard := range keyspace.Shards {
		for _, tablet := range shard.Tablets {
			if tablet.Vttablet.GetTabletStatus() == "SERVING" && strings.EqualFold(tablet.Vttablet.VreplicationTabletType, tabletType) {
				log.Infof("Serving status of tablet %s is %s, %s", tablet.Name, tablet.Vttablet.ServingStatus, tablet.Vttablet.GetTabletStatus())
				tablets[tablet.Name] = tablet.Vttablet
			}
		}
	}
	return tablets
}
