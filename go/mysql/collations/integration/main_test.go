/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"testing"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/vt/vttest"

	vttestpb "vitess.io/vitess/go/vt/proto/vttest"
)

var (
	connParams mysql.ConnParams
)

var waitmysql = flag.Bool("waitmysql", false, "")

var defaultenv = collations.Local()

func mysqlconn(t *testing.T) *mysql.Conn {
	conn, err := mysql.Connect(context.Background(), &connParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(conn.ServerVersion, "8.0.") {
		conn.Close()
		t.Skipf("collation integration tests are only supported in MySQL 8.0+")
	}
	return conn
}

func TestMain(m *testing.M) {
	flag.Parse()

	exitCode := func() int {
		// Launch MySQL.
		// We need a Keyspace in the topology, so the DbName is set.
		// We need a Shard too, so the database 'vttest' is created.
		cfg := vttest.Config{
			Topology: &vttestpb.VTTestTopology{
				Keyspaces: []*vttestpb.Keyspace{
					{
						Name: "vttest",
						Shards: []*vttestpb.Shard{
							{
								Name:           "0",
								DbNameOverride: "vttest",
							},
						},
					},
				},
			},
			OnlyMySQL: true,
		}
		cluster := vttest.LocalCluster{
			Config: cfg,
		}
		if err := cluster.Setup(); err != nil {
			fmt.Fprintf(os.Stderr, "could not launch mysql: %v\n", err)
			return 1
		}
		defer cluster.TearDown()

		connParams = cluster.MySQLConnParams()

		if *waitmysql {
			debugMysql()
		}
		return m.Run()
	}()
	os.Exit(exitCode)
}

func debugMysql() {
	fmt.Fprintf(os.Stderr, "Connect to MySQL using parameters: mysql -u %s -D %s -S %s\n",
		connParams.Uname, connParams.DbName, connParams.UnixSocket)
	fmt.Fprintf(os.Stderr, "Press ^C to resume testing...\n")

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	<-sigchan
	signal.Stop(sigchan)

	fmt.Fprintf(os.Stderr, "Resuming!\n")
}
