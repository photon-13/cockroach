// Copyright 2016 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/LICENSE

package sqlccl

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl/engineccl"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
)

func bankStatementBuf(numAccounts int) *bytes.Buffer {
	var buf bytes.Buffer
	buf.WriteString(bankCreateTable)
	buf.WriteString(";\n")
	stmts := bankDataInsertStmts(numAccounts)
	for _, s := range stmts {
		buf.WriteString(s)
		buf.WriteString(";\n")
	}
	return &buf
}

func BenchmarkClusterBackup(b *testing.B) {
	// NB: This benchmark takes liberties in how b.N is used compared to the go
	// documentation's description. We're getting useful information out of it,
	// but this is not a pattern to cargo-cult.
	defer tracing.Disable()()

	ctx, dir, _, sqlDB, cleanupFn := backupRestoreTestSetup(b, multiNode, 0)
	defer cleanupFn()
	sqlDB.Exec(`DROP TABLE bench.bank`)

	ts := hlc.Timestamp{WallTime: hlc.UnixNano()}
	loadDir := filepath.Join(dir, "load")
	if _, err := Load(ctx, sqlDB.DB, bankStatementBuf(b.N), "bench", loadDir, ts, 0, dir); err != nil {
		b.Fatalf("%+v", err)
	}
	sqlDB.Exec(fmt.Sprintf(`RESTORE bench.* FROM '%s'`, loadDir))

	// TODO(dan): Ideally, this would split and rebalance the ranges in a more
	// controlled way. A previous version of this code did it manually with
	// `SPLIT AT` and TestCluster's TransferRangeLease, but it seemed to still
	// be doing work after returning, which threw off the timing and the results
	// of the benchmark. DistSQL is working on improving this infrastructure, so
	// use what they build.

	b.ResetTimer()
	var unused string
	var dataSize int64
	sqlDB.QueryRow(fmt.Sprintf(`BACKUP DATABASE bench TO '%s'`, dir)).Scan(
		&unused, &unused, &unused, &dataSize,
	)
	b.StopTimer()
	b.SetBytes(dataSize / int64(b.N))
}

func BenchmarkClusterRestore(b *testing.B) {
	// NB: This benchmark takes liberties in how b.N is used compared to the go
	// documentation's description. We're getting useful information out of it,
	// but this is not a pattern to cargo-cult.
	defer tracing.Disable()()

	ctx, dir, _, sqlDB, cleanup := backupRestoreTestSetup(b, multiNode, 0)
	defer cleanup()
	sqlDB.Exec(`DROP TABLE bench.bank`)

	ts := hlc.Timestamp{WallTime: hlc.UnixNano()}
	backup, err := Load(ctx, sqlDB.DB, bankStatementBuf(b.N), "bench", dir, ts, 0, dir)
	if err != nil {
		b.Fatalf("%+v", err)
	}
	b.SetBytes(backup.DataSize / int64(b.N))
	b.ResetTimer()
	sqlDB.Exec(fmt.Sprintf(`RESTORE bench.* FROM '%s'`, dir))
	b.StopTimer()
}

func BenchmarkLoadRestore(b *testing.B) {
	// NB: This benchmark takes liberties in how b.N is used compared to the go
	// documentation's description. We're getting useful information out of it,
	// but this is not a pattern to cargo-cult.
	defer tracing.Disable()()

	ctx, dir, _, sqlDB, cleanup := backupRestoreTestSetup(b, multiNode, 0)
	defer cleanup()
	sqlDB.Exec(`DROP TABLE bench.bank`)

	buf := bankStatementBuf(b.N)
	b.SetBytes(int64(buf.Len() / b.N))
	ts := hlc.Timestamp{WallTime: hlc.UnixNano()}
	b.ResetTimer()
	if _, err := Load(ctx, sqlDB.DB, buf, "bench", dir, ts, 0, dir); err != nil {
		b.Fatalf("%+v", err)
	}
	sqlDB.Exec(fmt.Sprintf(`RESTORE bench.* FROM '%s'`, dir))
	b.StopTimer()
}

func BenchmarkLoadSQL(b *testing.B) {
	// NB: This benchmark takes liberties in how b.N is used compared to the go
	// documentation's description. We're getting useful information out of it,
	// but this is not a pattern to cargo-cult.
	_, _, _, sqlDB, cleanup := backupRestoreTestSetup(b, multiNode, 0)
	defer cleanup()
	sqlDB.Exec(`DROP TABLE bench.bank`)

	buf := bankStatementBuf(b.N)
	b.SetBytes(int64(buf.Len() / b.N))
	lines := make([]string, 0, b.N)
	for {
		line, err := buf.ReadString(';')
		if err == io.EOF {
			break
		} else if err != nil {
			b.Fatalf("%+v", err)
		}
		lines = append(lines, line)
	}

	b.ResetTimer()
	for _, line := range lines {
		sqlDB.Exec(line)
	}
	b.StopTimer()
}

func runEmptyIncrementalBackup(b *testing.B) {
	defer tracing.Disable()()

	const numStatements = 100000

	ctx, dir, _, sqlDB, cleanupFn := backupRestoreTestSetup(b, multiNode, 0)
	defer cleanupFn()

	restoreDir := filepath.Join(dir, "restore")
	fullDir := filepath.Join(dir, "full")

	ts := hlc.Timestamp{WallTime: hlc.UnixNano()}
	if _, err := Load(
		ctx, sqlDB.DB, bankStatementBuf(numStatements), "bench", restoreDir, ts, 0, restoreDir,
	); err != nil {
		b.Fatalf("%+v", err)
	}
	sqlDB.Exec(`DROP TABLE bench.bank`)
	sqlDB.Exec(`RESTORE bench.* FROM $1`, restoreDir)

	var unused string
	var dataSize int64
	sqlDB.QueryRow(`BACKUP DATABASE bench TO $1`, fullDir).Scan(
		&unused, &unused, &unused, &dataSize,
	)

	// We intentionally don't write anything to the database between the full and
	// incremental backup.

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		incrementalDir := filepath.Join(dir, fmt.Sprintf("incremental%d", i))
		sqlDB.Exec(`BACKUP DATABASE bench TO $1 INCREMENTAL FROM $2`, incrementalDir, fullDir)
	}
	b.StopTimer()

	// We report the number of bytes that incremental backup was able to
	// *skip*--i.e., the number of bytes in the full backup.
	b.SetBytes(int64(b.N) * dataSize)
}

func BenchmarkClusterEmptyIncrementalBackup(b *testing.B) {
	b.Run("Normal", func(b *testing.B) {
		defer settings.TestingSetBool(&engineccl.TimeBoundIteratorsEnabled, false)()
		runEmptyIncrementalBackup(b)
	})

	b.Run("TimeBound", func(b *testing.B) {
		defer settings.TestingSetBool(&engineccl.TimeBoundIteratorsEnabled, true)()
		runEmptyIncrementalBackup(b)
	})
}
