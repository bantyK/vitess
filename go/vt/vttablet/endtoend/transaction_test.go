/*
Copyright 2019 The Vitess Authors.

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

package endtoend

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/test/utils"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/vttablet/endtoend/framework"
	"vitess.io/vitess/go/vt/vttablet/tabletserver"
)

func TestCommit(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	vstart := framework.DebugVars()

	query := "insert into vitess_test (intval, floatval, charval, binval) values (4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)

	_, err = client.Execute(query, nil)
	require.NoError(t, err)

	err = client.Commit()
	require.NoError(t, err)

	qr, err := client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	require.Equal(t, 4, len(qr.Rows), "rows affected")

	_, err = client.Execute("delete from vitess_test where intval=4", nil)
	require.NoError(t, err)

	qr, err = client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	require.Equal(t, 3, len(qr.Rows), "rows affected")

	expectedDiffs := []struct {
		tag  string
		diff int
	}{{
		tag:  "Transactions/TotalCount",
		diff: 2,
	}, {
		tag:  "Transactions/Histograms/commit/Count",
		diff: 2,
	}, {
		tag:  "Queries/TotalCount",
		diff: 6,
	}, {
		tag:  "Queries/Histograms/BEGIN/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/COMMIT/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/Insert/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/DeleteLimit/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/Select/Count",
		diff: 2,
	}}
	vend := framework.DebugVars()
	for _, expected := range expectedDiffs {
		compareIntDiff(t, vend, expected.tag, vstart, expected.diff)
	}
}

func TestRollback(t *testing.T) {
	client := framework.NewClient()

	vstart := framework.DebugVars()

	query := "insert into vitess_test values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)
	err = client.Rollback()
	require.NoError(t, err)

	qr, err := client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	assert.Equal(t, 3, len(qr.Rows))

	expectedDiffs := []struct {
		tag  string
		diff int
	}{{
		tag:  "Transactions/TotalCount",
		diff: 1,
	}, {
		tag:  "Transactions/Histograms/rollback/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/BEGIN/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/ROLLBACK/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/Insert/Count",
		diff: 1,
	}}
	vend := framework.DebugVars()
	for _, expected := range expectedDiffs {
		compareIntDiff(t, vend, expected.tag, vstart, expected.diff)
	}
}

func TestAutoCommit(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	vstart := framework.DebugVars()

	query := "insert into vitess_test (intval, floatval, charval, binval) values (4, null, null, null)"
	_, err := client.Execute(query, nil)
	require.NoError(t, err)

	qr, err := client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	assert.Equal(t, 4, len(qr.Rows))

	_, err = client.Execute("delete from vitess_test where intval=4", nil)
	require.NoError(t, err)

	qr, err = client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	assert.Equal(t, 3, len(qr.Rows))

	expectedDiffs := []struct {
		tag  string
		diff int
	}{{
		tag:  "Transactions/TotalCount",
		diff: 2,
	}, {
		tag:  "Transactions/Histograms/commit/Count",
		diff: 2,
	}, {
		tag:  "Queries/TotalCount",
		diff: 4,
	}, {
		tag:  "Queries/Histograms/BEGIN/Count",
		diff: 0,
	}, {
		tag:  "Queries/Histograms/COMMIT/Count",
		diff: 0,
	}, {
		tag:  "Queries/Histograms/Insert/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/DeleteLimit/Count",
		diff: 1,
	}, {
		tag:  "Queries/Histograms/Select/Count",
		diff: 2,
	}}
	vend := framework.DebugVars()
	for _, expected := range expectedDiffs {
		got := framework.FetchInt(vend, expected.tag)
		want := framework.FetchInt(vstart, expected.tag) + expected.diff
		// It's possible that other house-keeping transactions (like messaging)
		// can happen during this test. So, don't perform equality comparisons.
		if got < want {
			t.Errorf("%s: %d, must be at least %d", expected.tag, got, want)
		}
	}
}

func TestForUpdate(t *testing.T) {
	for _, mode := range []string{"for update", "lock in share mode"} {
		client := framework.NewClient()
		query := fmt.Sprintf("select * from vitess_test where intval=2 %s", mode)
		_, err := client.Execute(query, nil)
		require.NoError(t, err)

		// We should not get errors here
		err = client.Begin(false)
		require.NoError(t, err)
		_, err = client.Execute(query, nil)
		require.NoError(t, err)
		err = client.Commit()
		require.NoError(t, err)
	}
}

func TestPrepareRollback(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)
	err = client.Prepare("aa")
	if err != nil {
		client.RollbackPrepared("aa", 0)
		t.Fatal(err.Error())
	}
	err = client.RollbackPrepared("aa", 0)
	require.NoError(t, err)
	qr, err := client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	assert.Equal(t, 3, len(qr.Rows))
}

func TestPrepareCommit(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)
	err = client.Prepare("aa")
	if err != nil {
		client.RollbackPrepared("aa", 0)
		t.Fatal(err)
	}
	err = client.CommitPrepared("aa")
	require.NoError(t, err)
	qr, err := client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	assert.Equal(t, 4, len(qr.Rows))
}

func TestPrepareReparentCommit(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)
	err = client.Prepare("aa")
	if err != nil {
		client.RollbackPrepared("aa", 0)
		t.Fatal(err)
	}
	// Rollback all transactions
	err = client.SetServingType(topodatapb.TabletType_REPLICA)
	require.NoError(t, err)
	// This should resurrect the prepared transaction.
	err = client.SetServingType(topodatapb.TabletType_PRIMARY)
	require.NoError(t, err)
	err = client.CommitPrepared("aa")
	require.NoError(t, err)
	qr, err := client.Execute("select * from vitess_test", nil)
	require.NoError(t, err)
	assert.Equal(t, 4, len(qr.Rows))
}

func TestShutdownGracePeriod(t *testing.T) {
	client := framework.NewClient()

	err := client.Begin(false)
	require.NoError(t, err)
	go func() {
		_, err := client.Execute("select sleep(10) from dual", nil)
		assert.Error(t, err)
	}()

	started := false
	for i := 0; i < 10; i++ {
		queries := framework.LiveQueryz()
		if len(queries) == 1 {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, started)

	start := time.Now()
	err = client.SetServingType(topodatapb.TabletType_REPLICA)
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 5*time.Second, time.Since(start))
	client.Rollback()

	client = framework.NewClientWithTabletType(topodatapb.TabletType_REPLICA)
	err = client.Begin(false)
	require.NoError(t, err)
	go func() {
		_, err := client.Execute("select sleep(11) from dual", nil)
		assert.Error(t, err)
	}()

	started = false
	for i := 0; i < 10; i++ {
		queries := framework.LiveQueryz()
		if len(queries) == 1 {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, started)
	start = time.Now()
	err = client.SetServingType(topodatapb.TabletType_PRIMARY)
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 1*time.Second, time.Since(start))
	client.Rollback()
}

func TestShutdownGracePeriodWithStreamExecute(t *testing.T) {
	client := framework.NewClient()

	err := client.Begin(false)
	require.NoError(t, err)
	go func() {
		_, err := client.StreamExecute("select sleep(10) from dual", nil)
		assert.Error(t, err)
	}()

	started := false
	for i := 0; i < 10; i++ {
		queries := framework.LiveQueryz()
		if len(queries) == 1 {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, started)

	start := time.Now()
	err = client.SetServingType(topodatapb.TabletType_REPLICA)
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 5*time.Second, time.Since(start))
	client.Rollback()

	client = framework.NewClientWithTabletType(topodatapb.TabletType_REPLICA)
	err = client.Begin(false)
	require.NoError(t, err)
	go func() {
		_, err := client.StreamExecute("select sleep(11) from dual", nil)
		assert.Error(t, err)
	}()

	started = false
	for i := 0; i < 10; i++ {
		queries := framework.LiveQueryz()
		if len(queries) == 1 {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, started)
	start = time.Now()
	err = client.SetServingType(topodatapb.TabletType_PRIMARY)
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 1*time.Second, time.Since(start))
	client.Rollback()
}

func TestShutdownGracePeriodWithReserveExecute(t *testing.T) {
	client := framework.NewClient()

	err := client.Begin(false)
	require.NoError(t, err)
	go func() {
		_, err := client.ReserveExecute("select sleep(10) from dual", nil, nil)
		assert.Error(t, err)
	}()

	started := false
	for i := 0; i < 10; i++ {
		queries := framework.LiveQueryz()
		if len(queries) == 1 {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, started)

	start := time.Now()
	err = client.SetServingType(topodatapb.TabletType_REPLICA)
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 5*time.Second, time.Since(start))
	client.Rollback()

	client = framework.NewClientWithTabletType(topodatapb.TabletType_REPLICA)
	err = client.Begin(false)
	require.NoError(t, err)
	go func() {
		_, err := client.ReserveExecute("select sleep(11) from dual", nil, nil)
		assert.Error(t, err)
	}()

	started = false
	for i := 0; i < 10; i++ {
		queries := framework.LiveQueryz()
		if len(queries) == 1 {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, started)
	start = time.Now()
	err = client.SetServingType(topodatapb.TabletType_PRIMARY)
	require.NoError(t, err)
	assert.True(t, time.Since(start) < 1*time.Second, time.Since(start))
	client.Rollback()
}

func TestShortTxTimeoutOltp(t *testing.T) {
	client := framework.NewClient()
	defer framework.Server.Config().SetTxTimeoutForWorkload(
		framework.Server.Config().TxTimeoutForWorkload(querypb.ExecuteOptions_OLTP),
		querypb.ExecuteOptions_OLTP,
	)
	framework.Server.Config().SetTxTimeoutForWorkload(10*time.Millisecond, querypb.ExecuteOptions_OLTP)

	err := client.Begin(false)
	require.NoError(t, err)
	start := time.Now()
	_, err = client.Execute("select sleep(10) from dual", nil)
	assert.Error(t, err)
	assert.True(t, time.Since(start) < 5*time.Second, time.Since(start))
	client.Rollback()
}

func TestShortTxTimeoutOlap(t *testing.T) {
	client := framework.NewClient()
	defer framework.Server.Config().SetTxTimeoutForWorkload(
		framework.Server.Config().TxTimeoutForWorkload(querypb.ExecuteOptions_OLAP),
		querypb.ExecuteOptions_OLAP,
	)
	framework.Server.Config().SetTxTimeoutForWorkload(10*time.Millisecond, querypb.ExecuteOptions_OLAP)

	err := client.Begin(false)
	require.NoError(t, err)
	start := time.Now()
	_, err = client.StreamExecute("select sleep(10) from dual", nil)
	assert.Error(t, err)
	assert.True(t, time.Since(start) < 5*time.Second, time.Since(start))
	client.Rollback()
}

func TestMMCommitFlow(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)

	err = client.CreateTransaction("aa", []*querypb.Target{{
		Keyspace: "test1",
		Shard:    "0",
	}, {
		Keyspace: "test2",
		Shard:    "1",
	}})
	require.NoError(t, err)

	err = client.CreateTransaction("aa", []*querypb.Target{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Duplicate entry")

	state, err := client.StartCommit("aa")
	require.NoError(t, err)
	assert.Equal(t, querypb.StartCommitState_Success, state)

	err = client.SetRollback("aa", 0)
	require.EqualError(t, err, "could not transition to ROLLBACK: aa (CallerID: dev)")

	info, err := client.ReadTransaction("aa")
	require.NoError(t, err)
	info.TimeCreated = 0
	wantInfo := &querypb.TransactionMetadata{
		Dtid:  "aa",
		State: querypb.TransactionState_COMMIT,
		Participants: []*querypb.Target{{
			Keyspace:   "test1",
			Shard:      "0",
			TabletType: topodatapb.TabletType_PRIMARY,
		}, {
			Keyspace:   "test2",
			Shard:      "1",
			TabletType: topodatapb.TabletType_PRIMARY,
		}},
	}
	utils.MustMatch(t, wantInfo, info, "ReadTransaction")

	err = client.ConcludeTransaction("aa")
	require.NoError(t, err)

	info, err = client.ReadTransaction("aa")
	require.NoError(t, err)
	wantInfo = &querypb.TransactionMetadata{}
	if !proto.Equal(info, wantInfo) {
		t.Errorf("ReadTransaction: %#v, want %#v", info, wantInfo)
	}
}

func TestMMRollbackFlow(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)

	err = client.CreateTransaction("aa", []*querypb.Target{{
		Keyspace: "test1",
		Shard:    "0",
	}, {
		Keyspace: "test2",
		Shard:    "1",
	}})
	require.NoError(t, err)
	client.Rollback()

	err = client.SetRollback("aa", 0)
	require.NoError(t, err)

	info, err := client.ReadTransaction("aa")
	require.NoError(t, err)
	info.TimeCreated = 0
	wantInfo := &querypb.TransactionMetadata{
		Dtid:  "aa",
		State: querypb.TransactionState_ROLLBACK,
		Participants: []*querypb.Target{{
			Keyspace:   "test1",
			Shard:      "0",
			TabletType: topodatapb.TabletType_PRIMARY,
		}, {
			Keyspace:   "test2",
			Shard:      "1",
			TabletType: topodatapb.TabletType_PRIMARY,
		}},
	}
	if !proto.Equal(info, wantInfo) {
		t.Errorf("ReadTransaction: %#v, want %#v", info, wantInfo)
	}

	err = client.ConcludeTransaction("aa")
	require.NoError(t, err)
}

type AsyncChecker struct {
	t  *testing.T
	ch chan bool
}

func newAsyncChecker(t *testing.T) *AsyncChecker {
	return &AsyncChecker{
		t:  t,
		ch: make(chan bool),
	}
}

func (ac *AsyncChecker) check() {
	ac.ch <- true
}

func (ac *AsyncChecker) shouldNotify(timeout time.Duration, message string) {
	select {
	case <-ac.ch:
		// notified, all is well
	case <-time.After(timeout):
		// timed out waiting for notification
		ac.t.Error(message)
	}
}
func (ac *AsyncChecker) shouldNotNotify(timeout time.Duration, message string) {
	select {
	case <-ac.ch:
		// notified - not expected
		ac.t.Error(message)
	case <-time.After(timeout):
		// timed out waiting for notification, which is expected
	}
}

// TestTransactionWatcherSignal test that unresolved transaction signal is received via health stream.
func TestTransactionWatcherSignal(t *testing.T) {
	client := framework.NewClient()

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)

	ch := newAsyncChecker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		err := client.StreamHealthWithContext(ctx, func(shr *querypb.StreamHealthResponse) error {
			if shr.RealtimeStats.TxUnresolved {
				ch.check()
			}
			return nil
		})
		require.NoError(t, err)
	}()

	err = client.CreateTransaction("aa", []*querypb.Target{
		{Keyspace: "test1", Shard: "0"},
		{Keyspace: "test2", Shard: "1"}})
	require.NoError(t, err)

	// wait for unresolved transaction signal
	ch.shouldNotify(2*time.Second, "timed out waiting for transaction watcher signal")

	err = client.SetRollback("aa", 0)
	require.NoError(t, err)

	// still should receive unresolved transaction signal
	ch.shouldNotify(2*time.Second, "timed out waiting for transaction watcher signal")

	err = client.ConcludeTransaction("aa")
	require.NoError(t, err)

	// transaction watcher should stop sending singal now.
	ch.shouldNotNotify(2*time.Second, "unexpected signal for resolved transaction")
}

func TestUnresolvedTracking(t *testing.T) {
	// This is a long running test. Enable only for testing the watchdog.
	t.Skip()
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	query := "insert into vitess_test (intval, floatval, charval, binval) " +
		"values(4, null, null, null)"
	err := client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute(query, nil)
	require.NoError(t, err)
	err = client.Prepare("aa")
	defer client.RollbackPrepared("aa", 0)
	require.NoError(t, err)
	time.Sleep(10 * time.Second)
	vars := framework.DebugVars()
	if val := framework.FetchInt(vars, "Unresolved/Prepares"); val != 1 {
		t.Errorf("Unresolved: %d, want 1", val)
	}
}

func TestManualTwopcz(t *testing.T) {
	// This is a manual test. Uncomment the Skip to perform this test.
	// The test will print the twopcz URL. Navigate to that location
	// and perform all the operations allowed. They should all succeed
	// and cause the transactions to be resolved.
	t.Skip()
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval=4", nil)

	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &connParams)
	require.NoError(t, err)
	defer conn.Close()

	// Successful prepare.
	err = client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute("insert into vitess_test (intval, floatval, charval, binval) values(4, null, null, null)", nil)
	require.NoError(t, err)
	_, err = client.Execute("insert into vitess_test (intval, floatval, charval, binval) values(5, null, null, null)", nil)
	require.NoError(t, err)
	err = client.Prepare("dtidsuccess")
	defer client.RollbackPrepared("dtidsuccess", 0)
	require.NoError(t, err)

	// Failed transaction.
	err = client.Begin(false)
	require.NoError(t, err)
	_, err = client.Execute("insert into vitess_test (intval, floatval, charval, binval) values(6, null, null, null)", nil)
	require.NoError(t, err)
	_, err = client.Execute("insert into vitess_test (intval, floatval, charval, binval) values(7, null, null, null)", nil)
	require.NoError(t, err)
	err = client.Prepare("dtidfail")
	defer client.RollbackPrepared("dtidfail", 0)
	require.NoError(t, err)
	conn.ExecuteFetch(fmt.Sprintf("update _vt.redo_state set state = %d where dtid = 'dtidfail'", tabletserver.RedoStateFailed), 10, false)
	conn.ExecuteFetch("commit", 10, false)

	// Distributed transaction.
	err = client.CreateTransaction("distributed", []*querypb.Target{{
		Keyspace: "k1",
		Shard:    "s1",
	}, {
		Keyspace: "k2",
		Shard:    "s2",
	}})
	defer client.ConcludeTransaction("distributed")

	require.NoError(t, err)
	fmt.Printf("%s/twopcz\n", framework.ServerAddress)
	fmt.Print("Sleeping for 30 seconds\n")
	time.Sleep(30 * time.Second)
}

// TestUnresolvedTransactions tests the UnresolvedTransactions API.
func TestUnresolvedTransactions(t *testing.T) {
	client := framework.NewClient()

	participants := []*querypb.Target{
		{Keyspace: "ks1", Shard: "80-c0", TabletType: topodatapb.TabletType_PRIMARY},
	}
	err := client.CreateTransaction("dtid01", participants)
	require.NoError(t, err)
	defer client.ConcludeTransaction("dtid01")

	// expected no transaction to show here, as 1 second not passed.
	transactions, err := client.UnresolvedTransactions()
	require.NoError(t, err)
	require.Empty(t, transactions)

	// abandon age is 1 second.
	time.Sleep(2 * time.Second)

	transactions, err = client.UnresolvedTransactions()
	require.NoError(t, err)
	want := []*querypb.TransactionMetadata{{
		Dtid:         "dtid01",
		State:        querypb.TransactionState_PREPARE,
		Participants: participants,
	}}

	require.Len(t, want, len(transactions))
	for i, transaction := range transactions {
		// Skipping check for TimeCreated
		assert.Equal(t, want[i].Dtid, transaction.Dtid)
		assert.Equal(t, want[i].State, transaction.State)
		assert.Equal(t, want[i].Participants, transaction.Participants)
	}
}

// TestUnresolvedTransactions tests the UnresolvedTransactions API.
func TestUnresolvedTransactionsOrdering(t *testing.T) {
	client := framework.NewClient()

	participants1 := []*querypb.Target{
		{Keyspace: "ks1", Shard: "c0-", TabletType: topodatapb.TabletType_PRIMARY},
		{Keyspace: "ks1", Shard: "80-c0", TabletType: topodatapb.TabletType_PRIMARY},
	}
	participants2 := []*querypb.Target{
		{Keyspace: "ks1", Shard: "-40", TabletType: topodatapb.TabletType_PRIMARY},
		{Keyspace: "ks1", Shard: "80-c0", TabletType: topodatapb.TabletType_PRIMARY},
	}
	participants3 := []*querypb.Target{
		{Keyspace: "ks1", Shard: "c0-", TabletType: topodatapb.TabletType_PRIMARY},
		{Keyspace: "ks1", Shard: "-40", TabletType: topodatapb.TabletType_PRIMARY},
	}
	// prepare state
	err := client.CreateTransaction("dtid01", participants1)
	require.NoError(t, err)
	defer client.ConcludeTransaction("dtid01")

	// commit state
	err = client.CreateTransaction("dtid02", participants2)
	require.NoError(t, err)
	defer client.ConcludeTransaction("dtid02")
	_, err = client.Execute(
		fmt.Sprintf("update _vt.dt_state set state = %d where dtid = 'dtid02'", querypb.TransactionState_COMMIT.Number()), nil)
	require.NoError(t, err)

	// rollback state
	err = client.CreateTransaction("dtid03", participants3)
	require.NoError(t, err)
	defer client.ConcludeTransaction("dtid03")
	_, err = client.Execute(
		fmt.Sprintf("update _vt.dt_state set state = %d where dtid = 'dtid03'", querypb.TransactionState_ROLLBACK.Number()), nil)
	require.NoError(t, err)

	// expected no transaction to show here, as 1 second not passed.
	transactions, err := client.UnresolvedTransactions()
	require.NoError(t, err)
	require.Empty(t, transactions)

	// abandon age is 1 second.
	time.Sleep(2 * time.Second)

	transactions, err = client.UnresolvedTransactions()
	require.NoError(t, err)
	want := []*querypb.TransactionMetadata{{
		Dtid:         "dtid02",
		State:        querypb.TransactionState_COMMIT,
		Participants: participants2,
	}, {
		Dtid:         "dtid03",
		State:        querypb.TransactionState_ROLLBACK,
		Participants: participants3,
	}, {
		Dtid:         "dtid01",
		State:        querypb.TransactionState_PREPARE,
		Participants: participants1,
	}}

	require.Len(t, want, len(transactions))
	for i, transaction := range transactions {
		// Skipping check for TimeCreated
		assert.Equal(t, want[i].Dtid, transaction.Dtid)
		assert.Equal(t, want[i].State, transaction.State)
		assert.Equal(t, want[i].Participants, transaction.Participants)
	}
}

// TestSkipUserMetrics tests the SkipUserMetrics flag in the config that disables user label in the metrics.
func TestSkipUserMetrics(t *testing.T) {
	client := framework.NewClient()
	query := "select * from vitess_test"

	runQueries := func() {
		// non-tx execute
		_, err := client.Execute(query, nil)
		require.NoError(t, err)

		// tx execute
		_, err = client.BeginExecute(query, nil, nil)
		require.NoError(t, err)
		require.NoError(t, client.Commit())
	}

	// Initial test with user metrics enabled
	vstart := framework.DebugVars()
	runQueries()

	expectedDiffs := []struct {
		tag  string
		diff int
	}{{ // not dependent on user
		tag: "Transactions/TotalCount", diff: 1,
	}, { // not dependent on user
		tag: "Transactions/Histograms/commit/Count", diff: 1,
	}, { // dependent on user
		tag: "TableACLAllowed/vitess_test.vitess_test.Select.dev", diff: 2,
	}, { // user metric enabled so this should be zero.
		tag: "TableACLAllowed/vitess_test.vitess_test.Select.UserLabelDisabled", diff: 0,
	}, { // dependent on user
		tag: "UserTableQueryCount/vitess_test.dev.Execute", diff: 2,
	}, { // user metric enabled so this should be zero.
		tag: "UserTableQueryCount/vitess_test.UserLabelDisabled.Execute", diff: 0,
	}, { // dependent on user
		tag: "UserTransactionCount/dev.commit", diff: 1,
	}}
	vend := framework.DebugVars()
	for _, expected := range expectedDiffs {
		compareIntDiff(t, vend, expected.tag, vstart, expected.diff)
	}

	// Enable SkipUserMetrics and re-run tests
	framework.Server.Config().SkipUserMetrics = true
	defer func() {
		framework.Server.Config().SkipUserMetrics = false
	}()
	vstart = framework.DebugVars()
	runQueries()

	expectedDiffs = []struct {
		tag  string
		diff int
	}{{ // not dependent on user
		tag: "Transactions/TotalCount", diff: 1,
	}, { // not dependent on user
		tag: "Transactions/Histograms/commit/Count", diff: 1,
	}, { // dependent on user - should be zero now
		tag: "TableACLAllowed/vitess_test.vitess_test.Select.dev", diff: 0,
	}, { // user metric disabled so this should be non-zero.
		tag: "TableACLAllowed/vitess_test.vitess_test.Select.UserLabelDisabled", diff: 2,
	}, { // dependent on user - should be zero now
		tag: "UserTableQueryCount/vitess_test.dev.Execute", diff: 0,
	}, { // user metric disabled so this should be non-zero.
		tag: "UserTableQueryCount/vitess_test.UserLabelDisabled.Execute", diff: 2,
	}, { // dependent on user
		tag: "UserTransactionCount/dev.commit", diff: 0,
	}, { // no need to publish this as "Transactions" histogram already captures this.
		tag: "UserTransactionCount/UserLabelDisabled.commit", diff: 0,
	}}
	vend = framework.DebugVars()
	for _, expected := range expectedDiffs {
		compareIntDiff(t, vend, expected.tag, vstart, expected.diff)
	}

}
