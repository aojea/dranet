/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apis

import (
	"fmt"
	"math"
	"testing"
)

func TestTableIDForName(t *testing.T) {
	// Names that one pod can use together: interface names (Alibaba bonds),
	// VRF names and MAC addresses (GCE NICs).
	var names []string
	for i := range 64 {
		names = append(names,
			fmt.Sprintf("eth%d", i),
			fmt.Sprintf("bond%d", i),
			fmt.Sprintf("vrf%d", i),
			fmt.Sprintf("vrf-%d", i),
			fmt.Sprintf("42:01:0a:00:00:%02x", i),
		)
	}

	tables := make(map[int]string, len(names))
	for _, name := range names {
		table := TableIDForName(name)
		if table < RouteTableOffset || table >= RouteTableOffset+RouteTableRange {
			t.Errorf("TableIDForName(%q) = %d, want a table in [%d, %d)", name, table, RouteTableOffset, RouteTableOffset+RouteTableRange)
		}
		if table > math.MaxInt32 {
			t.Errorf("TableIDForName(%q) = %d, want a table that fits in an int32", name, table)
		}
		if again := TableIDForName(name); again != table {
			t.Errorf("TableIDForName(%q) = %d, then %d, want a deterministic table", name, table, again)
		}
		if other, ok := tables[table]; ok {
			t.Errorf("TableIDForName(%q) = TableIDForName(%q) = %d, want different tables", name, other, table)
		}
		tables[table] = name
	}
}
