package queue

import (
	"encoding/json"
	"testing"
)

func TestTaskMessageJSONShape(t *testing.T) {
	msg := TaskMessage{
		TaskID:       "abc123",
		Type:         "osgb->3dtiles",
		Source:       "/data/osgb/town",
		Output:       "/data/out/town",
		ManifestPath: "/data/out/town/manifest.json",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 队列契约字段名固定：task_id/type/source/output/manifest_path
	for _, key := range []string{"task_id", "type", "source", "output", "manifest_path"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("TaskMessage JSON missing key %q: %s", key, data)
		}
	}

	var back TaskMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	// Params 为 map 不可直接比较，用 JSON 规范化对比
	backData, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(backData) != string(data) {
		t.Fatalf("roundtrip mismatch: %s vs %s", backData, data)
	}
}

func TestTaskMessageParamsRoundtrip(t *testing.T) {
	msg := TaskMessage{TaskID: "p1", Params: map[string]any{"lod": float64(2), "crs": "EPSG:4490"}}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back TaskMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Params["lod"] != float64(2) || back.Params["crs"] != "EPSG:4490" {
		t.Fatalf("params roundtrip mismatch: %+v", back.Params)
	}
}

func TestConnectFailsFastOnBadURL(t *testing.T) {
	// 无本地 NATS 时应快速失败返回 error（调用方据此降级），而非挂起
	if _, err := Connect("nats://127.0.0.1:1"); err == nil {
		t.Fatal("Connect to unreachable NATS should return error")
	}
}

func TestDefaultURL(t *testing.T) {
	if DefaultURL != "nats://127.0.0.1:14222" {
		t.Fatalf("DefaultURL = %q", DefaultURL)
	}
}
