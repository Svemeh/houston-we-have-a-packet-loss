package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func TestLoadObserverPrecedence(t *testing.T) {
	t.Chdir(t.TempDir()) // no stray .env
	settingsPath := filepath.Join(t.TempDir(), "settings.json")

	t.Setenv(envLatitude, "")
	t.Setenv(envLongitude, "")
	if _, source, err := LoadObserver(settingsPath); err != nil || source != "" {
		t.Fatalf("nothing set: source=%q err=%v, want no location and no error", source, err)
	}

	if err := SaveSettings(settingsPath, Settings{Location: &Observer{LatitudeDeg: 59.91, LongitudeDeg: 10.75}}); err != nil {
		t.Fatal(err)
	}
	observer, source, err := LoadObserver(settingsPath)
	if err != nil || source != LocationFromSettings || observer.LatitudeDeg != 59.91 {
		t.Fatalf("settings only: %+v source=%q err=%v", observer, source, err)
	}

	t.Setenv(envLatitude, "60.5")
	t.Setenv(envLongitude, "11")
	observer, source, err = LoadObserver(settingsPath)
	if err != nil || source != LocationFromEnv || observer.LatitudeDeg != 60.5 {
		t.Fatalf("env should win: %+v source=%q err=%v", observer, source, err)
	}

	t.Setenv(envLongitude, "")
	if _, _, err := LoadObserver(settingsPath); err == nil {
		t.Error("only one of lat/lon set: want an error")
	}

	t.Setenv(envLatitude, "95")
	t.Setenv(envLongitude, "10")
	if _, _, err := LoadObserver(settingsPath); err == nil {
		t.Error("latitude out of range: want an error")
	}
}

func TestLocationHandler(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "data", "settings.json")
	tracker := NewSkyTracker(Observer{}, "")
	handler := newLocationHandler(tracker, settingsPath)

	post := func(contentType, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, RouteLocation, strings.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		recorder := httptest.NewRecorder()
		handler(recorder, request)
		return recorder
	}

	if got := post("text/plain", `{"latitude_deg":1,"longitude_deg":2}`).Code; got != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON POST: status %d, want 415", got)
	}
	if got := post("application/json", `{"latitude_deg":91,"longitude_deg":2}`).Code; got != http.StatusBadRequest {
		t.Errorf("out-of-range POST: status %d, want 400", got)
	}
	if snapshot := tracker.Snapshot(testNow); snapshot.LocationSet {
		t.Fatal("location set before a valid POST")
	}

	if got := post("application/json", `{"latitude_deg":59.91234,"longitude_deg":10.75678}`); got.Code != http.StatusOK {
		t.Fatalf("valid POST: status %d: %s", got.Code, got.Body)
	}
	observer, source := tracker.Location()
	if source != LocationFromSettings || observer.LatitudeDeg != 59.91 || observer.LongitudeDeg != 10.76 {
		t.Errorf("tracker location = %+v from %q, want 59.91, 10.76 from settings", observer, source)
	}
	if !tracker.Snapshot(testNow).LocationSet {
		t.Error("snapshot still says no location after POST")
	}
	settings, err := LoadSettings(settingsPath)
	if err != nil || settings.Location == nil || *settings.Location != observer {
		t.Errorf("saved settings = %+v (err %v), want %+v", settings.Location, err, observer)
	}

	envTracker := NewSkyTracker(Observer{LatitudeDeg: 1, LongitudeDeg: 2}, LocationFromEnv)
	request := httptest.NewRequest(http.MethodPost, RouteLocation, strings.NewReader(`{"latitude_deg":3,"longitude_deg":4}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	newLocationHandler(envTracker, settingsPath)(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Errorf("POST over an env location: status %d, want 409", recorder.Code)
	}
}
