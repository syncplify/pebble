// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/internal/datadriven"
	"github.com/petermattis/pebble/storage"
)

func TestSnapshotListToSlice(t *testing.T) {
	testCases := []struct {
		vals []uint64
	}{
		{nil},
		{[]uint64{1}},
		{[]uint64{1, 2, 3}},
		{[]uint64{3, 2, 1}},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			var l snapshotList
			l.init()
			for _, v := range c.vals {
				l.pushBack(&Snapshot{seqNum: v})
			}
			slice := l.toSlice()
			if !reflect.DeepEqual(c.vals, slice) {
				t.Fatalf("expected %d, but got %d", c.vals, slice)
			}
		})
	}
}

func TestSnapshot(t *testing.T) {
	var d *DB
	var snapshots map[string]*Snapshot

	datadriven.RunTest(t, "testdata/snapshot", func(td *datadriven.TestData) string {
		switch td.Cmd {
		case "define":
			var err error
			d, err = Open("", &db.Options{
				Storage: storage.NewMem(),
			})
			if err != nil {
				t.Fatal(err)
			}
			snapshots = make(map[string]*Snapshot)

			for _, line := range strings.Split(td.Input, "\n") {
				parts := strings.Fields(line)
				if len(parts) == 0 {
					continue
				}
				var err error
				switch parts[0] {
				case "set":
					if len(parts) != 3 {
						t.Fatalf("%s expects 2 arguments", parts[0])
					}
					err = d.Set([]byte(parts[1]), []byte(parts[2]), nil)
				case "del":
					if len(parts) != 2 {
						t.Fatalf("%s expects 1 argument", parts[0])
					}
					err = d.Delete([]byte(parts[1]), nil)
				case "merge":
					if len(parts) != 3 {
						t.Fatalf("%s expects 2 arguments", parts[0])
					}
					err = d.Merge([]byte(parts[1]), []byte(parts[2]), nil)
				case "snapshot":
					if len(parts) != 2 {
						t.Fatalf("%s expects 1 argument", parts[0])
					}
					snapshots[parts[1]] = d.NewSnapshot()
				case "compact":
					if len(parts) != 2 {
						t.Fatalf("%s expects 1 argument", parts[0])
					}
					keys := strings.Split(parts[1], "-")
					if len(keys) != 2 {
						t.Fatalf("malformed key range: %s", parts[1])
					}
					err = d.Compact([]byte(keys[0]), []byte(keys[1]))
				default:
					t.Fatalf("unknown op: %s", parts[0])
				}
				if err != nil {
					t.Fatal(err)
				}
			}

		case "iter":
			var iter db.Iterator
			if len(td.CmdArgs) == 1 {
				if td.CmdArgs[0].Key != "snapshot" {
					t.Fatalf("unknown argument: %s", td.CmdArgs[0])
				}
				if len(td.CmdArgs[0].Vals) != 1 {
					t.Fatalf("%s expects 1 value: %s", td.CmdArgs[0].Key, td.CmdArgs[0])
				}
				name := td.CmdArgs[0].Vals[0]
				snapshot := snapshots[name]
				if snapshot == nil {
					return fmt.Sprintf("unable to find snapshot \"%s\"", name)
				}
				iter = snapshot.NewIter(nil)
			} else {
				iter = d.NewIter(nil)
			}
			defer iter.Close()

			var b bytes.Buffer
			for _, line := range strings.Split(td.Input, "\n") {
				parts := strings.Fields(line)
				if len(parts) == 0 {
					continue
				}
				switch parts[0] {
				case "first":
					iter.First()
				case "last":
					iter.Last()
				case "seek-ge":
					if len(parts) != 2 {
						return fmt.Sprintf("seek-ge <key>\n")
					}
					iter.SeekGE([]byte(strings.TrimSpace(parts[1])))
				case "seek-lt":
					if len(parts) != 2 {
						return fmt.Sprintf("seek-lt <key>\n")
					}
					iter.SeekLT([]byte(strings.TrimSpace(parts[1])))
				case "next":
					iter.Next()
				case "prev":
					iter.Prev()
				default:
					return fmt.Sprintf("unknown op: %s", parts[0])
				}
				if iter.Valid() {
					fmt.Fprintf(&b, "%s:%s\n", iter.Key(), iter.Value())
				} else if err := iter.Error(); err != nil {
					fmt.Fprintf(&b, "err=%v\n", err)
				} else {
					fmt.Fprintf(&b, ".\n")
				}
			}
			return b.String()

		default:
			t.Fatalf("unknown command: %s", td.Cmd)
		}
		return ""
	})
}