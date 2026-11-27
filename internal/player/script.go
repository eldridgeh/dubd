package player

import (
	"encoding/json"
	"fmt"
)

// The scripts run in the page through cmux's browser eval. Each is a single
// self-contained expression that keeps nothing in the page between calls and
// returns a JSON string, so it behaves the same whichever JavaScript world
// cmux evaluates it in.

// want is what the page's media element should be doing.
type want struct {
	Rate      float64 `json:"rate"`
	KeepPitch bool    `json:"keepPitch"`
	Play      bool    `json:"play"`
}

// page is what the tick script reports back.
type page struct {
	Host     string  `json:"host"`
	Path     string  `json:"path"`
	List     string  `json:"list"`
	Video    bool    `json:"video"`
	Ad       bool    `json:"ad"`
	SignedIn bool    `json:"signedIn"`
	Loaded   bool    `json:"loaded"`
	Paused   bool    `json:"paused"`
	Rate     float64 `json:"rate"`
	Time     float64 `json:"time"`
	Duration float64 `json:"duration"`
	// Hidden is whether the page is not on show.
	Hidden bool   `json:"hidden"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
}

// Track is one track: a search result, or something that was played.
type Track struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// By is what the page says beside the title, such as the artist.
	By string `json:"by,omitempty"`
	// Link plays the track by itself. Filled in where tracks are kept.
	Link string `json:"link,omitempty"`
}

// found is what the find script reports back.
type found struct {
	Tracks []Track `json:"tracks"`
}

const tickJS = `(() => {
  const want = %s;
  const out = {host: location.hostname, path: location.pathname, video: false};
  const params = new URLSearchParams(location.search);
  out.list = params.get('list') || '';
  out.id = params.get('v') || '';
  if (!out.id) {
    const link = document.querySelector('a.ytp-title-link[href*="v="]');
    const m = link && /[?&]v=([\w-]{11})/.exec(link.getAttribute('href') || '');
    if (m) out.id = m[1];
  }
  const text = (sel) => { const el = document.querySelector(sel); return el ? el.textContent.trim().replace(/\s+/g, ' ') : ''; };
  for (const b of document.querySelectorAll('ytmusic-you-there-renderer button, yt-confirm-dialog-renderer #confirm-button button')) {
    if (b.offsetParent !== null) b.click();
  }
  // An advert: take the skip button when YouTube offers one.
  out.ad = !!document.querySelector('.ad-showing, .ad-interrupting');
  if (out.ad) {
    for (const b of document.querySelectorAll('.ytp-skip-ad-button, .ytp-ad-skip-button, .ytp-ad-skip-button-modern')) {
      if (b.offsetParent !== null) b.click();
    }
  }
  const v = document.querySelector('video');
  if (v) {
    out.video = true;
    out.loaded = !!(v.currentSrc || v.src);
    for (const k of ['preservesPitch', 'webkitPreservesPitch']) { if (k in v) v[k] = want.keepPitch; }
    // A dip in volume that was cut short, by an error or by dubd stopping
    // part way, is put right here. dubd runs a dip from start to end before
    // it runs this script again, so a volume still kept on the element
    // means the dip did not finish.
    if (v.dataset.dubdVol) {
      v.volume = Number(v.dataset.dubdVol);
      delete v.dataset.dubdVol;
    }
    // With nothing to be heard the rate is simply set. While sound is
    // playing dubd changes it inside a dip in volume (see dipJS), which it
    // starts when it sees the rate reported here is not the one wanted.
    if (Math.abs(v.playbackRate - want.rate) > 0.0005 && (v.paused || v.muted || v.volume === 0)) {
      v.playbackRate = want.rate;
    }
    if (want.play && v.paused && out.loaded) { const p = v.play(); if (p && p.catch) p.catch(() => {}); }
    if (!want.play && !v.paused) v.pause();
    out.paused = v.paused;
    out.rate = v.playbackRate;
    out.time = v.currentTime || 0;
    out.duration = isFinite(v.duration) ? v.duration : 0;
  }
  out.hidden = document.hidden;
  out.signedIn = !document.querySelector('ytmusic-nav-bar a.sign-in-link, a[href*="accounts.google.com/ServiceLogin"]');
  out.title = text('ytmusic-player-bar .title') || document.title;
  // The artist: what the page tells the system's media controls, which
  // does not depend on how the player bar is laid out, then the bar's
  // byline, which reads "artist • album • year".
  const meta = (navigator.mediaSession && navigator.mediaSession.metadata) || {};
  out.artist = (meta.artist || '').trim() || text('ytmusic-player-bar .byline a') || text('ytmusic-player-bar .byline').split(' • ')[0];
  return JSON.stringify(out);
})()`

const findJS = `(() => {
  const clean = (el) => el ? el.textContent.trim().replace(/\s+/g, ' ') : '';
  const tracks = [];
  for (const a of document.querySelectorAll('a[href*="watch?v="]')) {
    const m = /[?&]v=([\w-]{11})/.exec(a.getAttribute('href') || '');
    if (!m) continue;
    const row = a.closest('ytmusic-responsive-list-item-renderer');
    // The line under the title is several pieces, such as "Song", the
    // artist and the play count.
    const by = row ? [...row.querySelectorAll('.secondary-flex-columns yt-formatted-string')].map(clean).filter(Boolean).join(' • ') : '';
    const t = {id: m[1], title: clean(a), by: by};
    const seen = tracks.find((x) => x.id === t.id);
    if (!seen) tracks.push(t);
    else if (!seen.title) { seen.title = t.title; seen.by = seen.by || t.by; }
  }
  return JSON.stringify({tracks: tracks});
})()`

// dipJS is one step of a change of tempo. WebKit drops the audio for a
// moment whenever the playback rate changes, so dubd takes the volume down
// in a few steps, changes the rate in the silence, and brings the volume
// back. Each step is one short script and dubd keeps the time between
// them: timers in the page cannot, because a tab that is not on show has
// its timers held to about one a second, which stretched the silence to
// two seconds. The volume to come back to is kept on the element.
const dipJS = `(() => {
  const dip = %s;
  const v = document.querySelector('video');
  if (!v) return JSON.stringify({ok: false});
  if (!v.dataset.dubdVol) v.dataset.dubdVol = String(v.volume);
  v.volume = Math.max(0, Math.min(1, Number(v.dataset.dubdVol) * dip.volume));
  if (dip.rate) v.playbackRate = dip.rate;
  if (dip.last) delete v.dataset.dubdVol;
  return JSON.stringify({ok: true});
})()`

// dipStep is one step of a dip: the share of the full volume to set, the
// rate to change to (0 leaves it), and whether the dip ends here.
type dipStep struct {
	Volume float64 `json:"volume"`
	Rate   float64 `json:"rate,omitempty"`
	Last   bool    `json:"last,omitempty"`
}

func dipScript(d dipStep) string {
	b, _ := json.Marshal(d)
	return fmt.Sprintf(dipJS, b)
}

func tickScript(w want) string {
	b, _ := json.Marshal(w)
	return fmt.Sprintf(tickJS, b)
}
