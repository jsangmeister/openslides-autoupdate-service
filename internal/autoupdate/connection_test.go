package autoupdate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/openslides/openslides-autoupdate-service/internal/autoupdate"
	"github.com/openslides/openslides-autoupdate-service/internal/test"
)

func TestConnect(t *testing.T) {
	closed := make(chan struct{})
	defer close(closed)
	c, _ := getConnection(closed)

	data, err := c.Next(context.Background())
	if err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}

	if value, ok := data["user/1/name"]; !ok || string(value) != `"Hello World"` {
		t.Errorf("c.Next() returned %v, expected map[user/1/name:\"Hello World\"", data)
	}
}

func TestConnectionReadNoNewData(t *testing.T) {
	closed := make(chan struct{})
	defer close(closed)
	c, _ := getConnection(closed)
	ctx, disconnect := context.WithCancel(context.Background())

	if _, err := c.Next(ctx); err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}

	disconnect()
	data, err := c.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("c.Next() returned error %v, expected context.Canceled", err)
	}
	if data != nil {
		t.Errorf("Expect no new data, got: %v", data)
	}
}

func TestConnectionReadNewData(t *testing.T) {
	closed := make(chan struct{})
	defer close(closed)
	c, datastore := getConnection(closed)

	if _, err := c.Next(context.Background()); err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}

	datastore.Update(map[string]json.RawMessage{"user/1/name": []byte(`"new value"`)})
	datastore.Send(test.Str("user/1/name"))
	data, err := c.Next(context.Background())

	if err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}
	if got := len(data); got != 1 {
		t.Errorf("Expected data to have one key, got: %d", got)
	}
	if value, ok := data["user/1/name"]; !ok || string(value) != `"new value"` {
		t.Errorf("c.Next() returned %v, expected %v", data, map[string]string{"user/1/name": `"new value"`})
	}
}

func TestConnectionEmptyData(t *testing.T) {
	const (
		doesNotExistKey = "doesnot/1/exist"
		doesExistKey    = "user/1/name"
	)

	datastore := new(test.MockDatastore)

	datastore.Data = map[string]json.RawMessage{
		doesExistKey: []byte("exist"),
	}
	datastore.OnlyData = true

	closed := make(chan struct{})
	defer close(closed)

	s := autoupdate.New(datastore, new(test.MockRestricter), closed)

	kb := mockKeysBuilder{keys: test.Str(doesExistKey, doesNotExistKey)}

	t.Run("First responce", func(t *testing.T) {
		c := s.Connect(1, kb, 0)

		data, err := c.Next(context.Background())

		if err != nil {
			t.Errorf("c.Next() returned an error: %v", err)
		}
		if _, ok := data[doesExistKey]; !ok {
			t.Errorf("key %s not in first responce", doesExistKey)
		}
		if _, ok := data[doesNotExistKey]; ok {
			t.Errorf("key %s is in first responce", doesNotExistKey)
		}
	})

	for _, tt := range []struct {
		name           string
		update         map[string]json.RawMessage
		expectExist    bool
		expectNotExist bool
	}{
		{
			"not exist->not exist",
			map[string]json.RawMessage{doesNotExistKey: nil},
			false, // existing key gets filtered.
			false,
		},
		{
			"not exist->exist",
			map[string]json.RawMessage{doesNotExistKey: []byte("value")},
			false, // existing key gets filtered.
			true,
		},
		{
			"exist->not exist",
			map[string]json.RawMessage{doesExistKey: nil},
			true,
			false,
		},
		{
			"exist->exist",
			map[string]json.RawMessage{doesExistKey: []byte("new value")},
			true,
			false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := s.Connect(1, kb, 0)
			if _, err := c.Next(context.Background()); err != nil {
				t.Errorf("c.Next() returned an error: %v", err)
			}

			datastore.Update(tt.update)
			datastore.Send([]string{doesExistKey, doesNotExistKey})
			data, err := c.Next(context.Background())

			if err != nil {
				t.Fatalf("c.Next() returned an error: %v", err)
			}
			if _, ok := data[doesExistKey]; ok != tt.expectExist {
				t.Errorf("key %s in second responce: %t, expect: %t", doesExistKey, ok, tt.expectExist)
			}
			if _, ok := data[doesNotExistKey]; ok != tt.expectNotExist {
				t.Errorf("key %s in second responce: %t, expect: %t", doesNotExistKey, ok, tt.expectExist)
			}

		})
	}

	t.Run("exit->not exist-> not exist", func(t *testing.T) {
		c := s.Connect(1, kb, 0)
		if _, err := c.Next(context.Background()); err != nil {
			t.Errorf("c.Next() returned an error: %v", err)
		}

		// First time not exist
		datastore.Update(map[string]json.RawMessage{doesExistKey: nil})
		datastore.Send([]string{doesExistKey})
		c.Next(context.Background())

		// Second time not exist
		datastore.Send([]string{doesExistKey})
		data, err := c.Next(context.Background())

		if err != nil {
			t.Fatalf("c.Next() returned an error: %v", err)
		}
		if _, ok := data[doesExistKey]; ok {
			t.Errorf("key %s in second responce: true, expect: false", doesExistKey)
		}
	})
}

func TestConnectionFilterData(t *testing.T) {
	datastore := new(test.MockDatastore)

	closed := make(chan struct{})
	defer close(closed)
	s := autoupdate.New(datastore, new(test.MockRestricter), closed)
	kb := mockKeysBuilder{keys: test.Str("user/1/name")}
	c := s.Connect(1, kb, 0)
	if _, err := c.Next(context.Background()); err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}

	datastore.Send(test.Str("user/1/name")) // send again, value did not change in restricter
	data, err := c.Next(context.Background())

	if err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}
	if got := len(data); got != 0 {
		t.Errorf("Got %d keys, expected none", got)
	}
	if _, ok := data["user/1/name"]; ok {
		t.Errorf("c.Next() returned %v, expected empty map", data)
	}
}

func TestConntectionFilterOnlyOneKey(t *testing.T) {
	datastore := new(test.MockDatastore)
	closed := make(chan struct{})
	close(closed)
	s := autoupdate.New(datastore, new(test.MockRestricter), closed)
	kb := mockKeysBuilder{keys: test.Str("user/1/name")}
	c := s.Connect(1, kb, 0)
	if _, err := c.Next(context.Background()); err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}

	datastore.Update(map[string]json.RawMessage{"user/1/name": []byte(`"newname"`)}) // Only change user/1 not user/2
	datastore.Send(test.Str("user/1/name", "user/2/name"))
	data, err := c.Next(context.Background())

	if err != nil {
		t.Errorf("c.Next() returned an error: %v", err)
	}
	if got := len(data); got != 1 {
		t.Errorf("Expected data to have one key, got: %d", got)
	}
	if _, ok := data["user/1/name"]; !ok {
		t.Errorf("Returned value does not have key `user/1/name`")
	}
	if got := string(data["user/1/name"]); got != `"newname"` {
		t.Errorf("Expect value `newname` got: %s", got)
	}
}

func BenchmarkFilterChanging(b *testing.B) {
	const keyCount = 100
	datastore := new(test.MockDatastore)
	closed := make(chan struct{})
	defer close(closed)
	s := autoupdate.New(datastore, new(test.MockRestricter), closed)

	keys := make([]string, 0, keyCount)
	for i := 0; i < keyCount; i++ {
		keys = append(keys, fmt.Sprintf("user/%d/name", i))
	}
	kb := mockKeysBuilder{keys: keys}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := s.Connect(1, kb, 0)

	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		c.Next(ctx)
		for i := 0; i < keyCount; i++ {
			datastore.Update(map[string]json.RawMessage{fmt.Sprintf("user/%d/name", i): []byte(fmt.Sprintf(`"value %d"`, n))})
		}
		datastore.Send(keys)
	}
}

func BenchmarkFilterNotChanging(b *testing.B) {
	const keyCount = 100
	datastore := new(test.MockDatastore)
	closed := make(chan struct{})
	defer close(closed)
	s := autoupdate.New(datastore, new(test.MockRestricter), closed)

	keys := make([]string, 0, keyCount)
	for i := 0; i < keyCount; i++ {
		keys = append(keys, fmt.Sprintf("user/%d/name", i))
	}
	kb := mockKeysBuilder{keys: keys}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := s.Connect(1, kb, 0)

	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		c.Next(ctx)
		datastore.Send(keys)
	}
}
