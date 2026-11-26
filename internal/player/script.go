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
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
}

// found is what the find script reports back.
type found struct {
	IDs []string `json:"ids"`
}

const tickJS = `(() => {
  const want = %s;
  const out = {host: location.hostname, path: location.pathname, video: false};
  out.list = new URLSearchParams(location.search).get('list') || '';
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
    if (Math.abs(v.playbackRate - want.rate) > 0.0005) {
      // WebKit drops the audio for a moment whenever the rate changes. Dip
      // the volume around the change so the drop lands in silence and is
      // heard as a short swell, not a gap. The dip state lives on the
      // element so that an overlapping call leaves it alone.
      if (v.paused || v.muted || v.volume === 0) {
        v.playbackRate = want.rate;
      } else if (!v.dataset.dubdDip) {
        const full = v.volume;
        // Timed by the clock, so a throttled timer shortens the ramp
        // instead of stretching it.
        const ramp = (from, to, ms, done) => {
          const start = performance.now();
          const id = setInterval(() => {
            const k = Math.min(1, (performance.now() - start) / ms);
            v.volume = Math.max(0, Math.min(1, from + (to - from) * k));
            if (k >= 1) { clearInterval(id); done(); }
          }, 15);
        };
        v.dataset.dubdDip = '1';
        ramp(full, 0, 120, () => {
          v.playbackRate = want.rate;
          setTimeout(() => ramp(0, full, 250, () => { delete v.dataset.dubdDip; }), 180);
        });
      }
    }
    if (want.play && v.paused && out.loaded) { const p = v.play(); if (p && p.catch) p.catch(() => {}); }
    if (!want.play && !v.paused) v.pause();
    out.paused = v.paused;
    out.rate = v.playbackRate;
    out.time = v.currentTime || 0;
    out.duration = isFinite(v.duration) ? v.duration : 0;
  }
  out.signedIn = !document.querySelector('ytmusic-nav-bar a.sign-in-link, a[href*="accounts.google.com/ServiceLogin"]');
  out.title = text('ytmusic-player-bar .title') || document.title;
  out.artist = text('ytmusic-player-bar .byline a');
  return JSON.stringify(out);
})()`

const findJS = `(() => {
  const ids = [];
  for (const a of document.querySelectorAll('a[href*="watch?v="]')) {
    const m = /[?&]v=([\w-]{11})/.exec(a.getAttribute('href') || '');
    if (m && !ids.includes(m[1])) ids.push(m[1]);
  }
  return JSON.stringify({ids: ids});
})()`

func tickScript(w want) string {
	b, _ := json.Marshal(w)
	return fmt.Sprintf(tickJS, b)
}
