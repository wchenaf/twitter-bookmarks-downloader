// ==UserScript==
// @name         Twitter Bookmarks Sync to Local
// @namespace    http://tampermonkey.net/
// @version      0.3
// @description  Intercept XHR to sync bookmarks, with auto-scroll and unattended periodic sync.
// @author       Gemini
// @match        https://x.com/*
// @match        https://twitter.com/*
// @run-at       document-start
// @grant        unsafeWindow
// @grant        GM_xmlhttpRequest
// ==/UserScript==

;(function () {
  'use strict'
  const RAW_SYNC_URL = 'http://localhost:41008/api/sync-raw'

  // How often an idle bookmarks tab reloads itself to pick up new bookmarks.
  const AUTO_RELOAD_MS = 30 * 60 * 1000
  // When the tab is in the foreground a reload would yank the page out from
  // under whoever is reading it, so retry after this instead.
  const AUTO_RETRY_MS = 5 * 60 * 1000
  // Consecutive failures at which a run gives up.
  const MAX_SYNC_FAILURES = 5
  // A POST that never completes counts as no failure at all, so a backend that
  // accepts the connection and then stalls would leave a scroll running with
  // nothing to bound it. The handler only touches the local database, media
  // downloads happening on a worker, so anything this slow is already broken.
  const SYNC_TIMEOUT_MS = 30 * 1000
  // x.com moved bookmarks from /i/bookmarks to /i/history in August 2026. The
  // old route is kept as well: the underlying GraphQL operation was not renamed
  // along with the route, which suggests a relabelling that could be reverted
  // or rolled out unevenly.
  const SYNC_PATHS = ['/i/bookmarks', '/i/history']

  const onSyncPage = () => SYNC_PATHS.some((p) => window.location.pathname.includes(p))

  console.log('[TBD v0.3] Terminal UI Edition loaded.')

  const UI = {
    el: null,
    btn: null,
    statusEl: null,
    forceBtn: null,
    timeout: null,
    isForce: false,

    init() {
      this.el = document.createElement('div')
      this.el.style.cssText = `
                position: fixed;
                bottom: 80px;
                left: 20px;
                background: #000;
                color: #0f0;
                padding: 10px 14px;
                border-radius: 4px;
                font-family: "Consolas", "Monaco", "Courier New", monospace;
                font-size: 12px;
                z-index: 999999;
                border: 1px solid #333;
                box-shadow: 0 0 10px rgba(0, 255, 0, 0.1);
                display: flex;
                flex-direction: column;
                gap: 8px;
                min-width: 130px;
                user-select: none;
                letter-spacing: 0.5px;
            `

      const controls = document.createElement('div')
      controls.style.cssText =
        'display: flex; justify-content: space-between; align-items: center; gap: 12px;'

      this.btn = document.createElement('div')
      this.btn.innerText = '[RUN]'
      this.btn.style.cssText =
        'cursor: pointer; font-weight: bold; color: #0f0; text-shadow: 0 0 2px rgba(0,255,0,0.5);'
      this.btn.onclick = () => Scroller.toggle()

      this.forceBtn = document.createElement('div')
      this.forceBtn.innerText = '[FORCE:OFF]'
      this.forceBtn.style.cssText = 'cursor: pointer; color: #666; font-size: 10px;'
      this.forceBtn.title = 'Toggle Force Mode'
      this.forceBtn.onclick = () => this.toggleForce()

      controls.appendChild(this.btn)
      controls.appendChild(this.forceBtn)

      this.statusEl = document.createElement('div')
      this.statusEl.innerText = '> SYSTEM READY'
      this.statusEl.style.cssText =
        'color: #0f0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 150px; border-top: 1px dashed #333; padding-top: 6px; font-size: 11px;'

      this.el.appendChild(controls)
      this.el.appendChild(this.statusEl)

      const monitor = () => {
        if (!document.body) {
          requestAnimationFrame(monitor)
          return
        }
        if (!this.el.parentElement) document.body.appendChild(this.el)

        if (onSyncPage()) {
          this.el.style.display = 'flex'
        } else {
          this.el.style.display = 'none'
          if (Scroller.active) Scroller.stop()
        }
        requestAnimationFrame(monitor)
      }
      monitor()
    },

    updateStatus(text, color = '#0f0') {
      this.statusEl.innerText = `> ${text}`
      this.statusEl.style.color = color

      if (color !== '#0f0' && color !== '#888') {
        if (this.timeout) clearTimeout(this.timeout)
        this.timeout = setTimeout(() => {
          if (Scroller.active) {
            const mode = this.isForce ? 'INFINITE' : 'SMART'
            this.statusEl.innerText = `> SCROLLING:${mode}`
            this.statusEl.style.color = '#0ff'
          } else {
            this.statusEl.innerText = '> SYSTEM READY'
            this.statusEl.style.color = '#0f0'
          }
        }, 2000)
      }
    },

    toggleForce() {
      this.isForce = !this.isForce
      this.forceBtn.innerText = this.isForce ? '[FORCE:ON]' : '[FORCE:OFF]'
      this.forceBtn.style.color = this.isForce ? '#ff9f00' : '#666'
      this.forceBtn.style.textShadow = this.isForce ? '0 0 2px #ff9f00' : 'none'

      if (Scroller.active) {
        this.statusEl.innerText = `> SCROLLING:${this.isForce ? 'INFINITE' : 'SMART'}`
      } else {
        this.updateStatus(
          this.isForce ? 'MODE:FORCE' : 'MODE:SMART',
          this.isForce ? '#ff9f00' : '#888'
        )
      }
    },

    setScrolling(isScrolling) {
      if (isScrolling) {
        this.btn.innerText = '[STOP]'
        this.btn.style.color = '#ff0033'
        this.btn.style.textShadow = '0 0 2px #ff0033'

        const mode = this.isForce ? 'INFINITE' : 'SMART'
        this.statusEl.innerText = `> SCROLLING:${mode}`
        this.statusEl.style.color = '#0ff' // Cyan for active state
        this.el.style.borderColor = '#0ff'
      } else {
        this.btn.innerText = '[RUN]'
        this.btn.style.color = '#0f0'
        this.btn.style.textShadow = '0 0 2px #0f0'

        this.statusEl.innerText = '> HALTED'
        this.statusEl.style.color = '#888'
        this.el.style.borderColor = '#333'
      }
    },

    isForceMode() {
      return this.isForce
    },
  }

  const Scroller = {
    active: false,
    timer: null,

    toggle() {
      if (this.active) this.stop()
      else this.start()
    },

    start() {
      if (this.active) return
      // A fresh run gets a fresh failure budget, so pressing RUN after fixing
      // the backend behaves like a first attempt rather than inheriting an
      // already-exhausted count.
      SyncFailures.reset()
      this.active = true
      UI.setScrolling(true)
      this.loop()
    },

    stop() {
      if (!this.active) return
      this.active = false
      clearTimeout(this.timer)
      UI.setScrolling(false)
    },

    loop() {
      if (!this.active) return
      window.scrollTo(0, document.body.scrollHeight)
      this.timer = setTimeout(() => {
        this.loop()
      }, 5000)
    },
  }

  // The only thing that ever stops a scroll is the backend saying it has seen
  // enough duplicates, so an unreachable backend means that signal never comes.
  // Unattended that would scroll on indefinitely, several requests a minute
  // against x.com under the user's own account, which is precisely the traffic
  // pattern worth not producing. Counting consecutive failures bounds it.
  // No cap is placed on a run that is succeeding: the backend still reporting
  // new tweets means the scroll is doing real work, and cutting that off would
  // truncate a legitimate backfill. State resets on reload, so recovering is
  // just a matter of fixing the backend and letting the next cycle come round.
  const SyncFailures = {
    count: 0,

    reset() {
      this.count = 0
    },

    note(label) {
      this.count++
      if (this.count < MAX_SYNC_FAILURES) {
        UI.updateStatus(`${label} ${this.count}/${MAX_SYNC_FAILURES}`, '#ff0033')
        return
      }
      Scroller.stop()
      // updateStatus reverts to the idle text after a couple of seconds, so the
      // reason a run died would otherwise leave no trace on a tab nobody is
      // watching.
      console.error(`[TBD] ${label} x${this.count}, run aborted.`)
      UI.updateStatus(`ABORTED: ${label}`, '#ff0033')
    },
  }

  // Unattended sync. Loading the bookmarks page issues the Bookmarks GraphQL
  // query on its own, and the interception below picks it up, so a periodic
  // reload is enough to stay current without anyone pressing RUN.
  //
  // A reload rather than a scroll because new bookmarks arrive at the top of the
  // timeline: a tab left open for hours is stale exactly where the new items
  // are, and scrolling only walks further back in time. Once the reload lands
  // and the backend reports something new, the scroller takes over from there.
  // Both work in a hidden tab, which was established by testing rather than
  // inferred from how browsers throttle background tabs.
  const AutoSync = {
    schedule(delay = AUTO_RELOAD_MS) {
      // requestAnimationFrame is frozen in a background tab, so nothing here may
      // depend on the UI monitor loop. setTimeout still fires, throttled to
      // roughly once a minute, which is ample at this cadence.
      setTimeout(() => this.fire(), delay)
    },

    fire() {
      // The path is re-checked at fire time rather than at load time so that
      // client-side navigation into or out of the bookmarks page is handled.
      if (!onSyncPage()) return this.schedule()
      if (!document.hidden) return this.schedule(AUTO_RETRY_MS)
      // An active scroll must not be reloaded out from under: scroll state and
      // FORCE mode live only in memory, so the run would silently restart in
      // SMART mode, stop at the duplicate cutoff, and never reach whatever lay
      // below the kill point.
      if (Scroller.active) {
        console.log('[TBD] Auto-reload deferred: scroll in progress.')
        return this.schedule()
      }
      window.location.reload()
    },
  }

  // Everything here hangs off one string match against an operation name that
  // x.com owns and can rename without notice, and the failure is silent: no
  // match simply means no sync. Naming the operations that were seen and passed
  // over turns the next rename from a mystery into a glance at the console. The
  // set keeps it to one line per operation instead of one per request.
  const ignoredOps = new Set()

  const noteIgnoredOp = (url) => {
    const op = url.split('?')[0].split('/').pop()
    if (!op || ignoredOps.has(op)) return
    ignoredOps.add(op)
    console.log(`[TBD] GraphQL operation seen and ignored: ${op}`)
  }

  const PageXHR = unsafeWindow.XMLHttpRequest
  const originalOpen = PageXHR.prototype.open
  const originalSend = PageXHR.prototype.send

  PageXHR.prototype.open = function (method, url) {
    try {
      this._gemini_url = url
    } catch (e) {}
    return originalOpen.apply(this, arguments)
  }

  PageXHR.prototype.send = function () {
    const self = this
    const onLoad = function () {
      self.removeEventListener('load', onLoad)
      const url = self._gemini_url

      if (typeof url === 'string' && url.includes('Bookmarks') && url.includes('graphql')) {
        const responseData = self.responseText
        if (responseData) {
          GM_xmlhttpRequest({
            method: 'POST',
            url: RAW_SYNC_URL,
            headers: { 'Content-Type': 'application/json' },
            data: responseData,
            timeout: SYNC_TIMEOUT_MS,
            onload: function (response) {
              try {
                const res = JSON.parse(response.responseText)
                if (res.duplicate_limit_reached) {
                  SyncFailures.reset()
                  if (!UI.isForceMode()) {
                    Scroller.stop()
                    UI.updateStatus('LIMIT REACHED', '#ff0033')
                  } else {
                    // Silent continuation in Force Mode
                    console.log(`[TBD] Limit hit (Force). Saved: ${res.saved_count}`)
                  }
                } else if (res.saved_count > 0) {
                  SyncFailures.reset()
                  UI.updateStatus(`SAVED:${res.saved_count}`, '#0f0')
                  // The backend did not find enough duplicates, so this page was
                  // largely new and more probably waits below it. A reload would
                  // only fetch the same first page again, so paginate by
                  // scrolling. Idempotent while a run is already in progress.
                  Scroller.start()
                } else {
                  // Neither new tweets nor the duplicate signal means the
                  // response carried no usable timeline, which is what x.com
                  // returns once it starts rate limiting a scroll. Reading that
                  // as "page was new, keep going" would answer a rate limit with
                  // more requests, so it spends the same budget as an
                  // unreachable backend rather than resetting it.
                  SyncFailures.note('NO DATA')
                }
              } catch (e) {
                SyncFailures.note('BACKEND ERR')
              }
            },
            onerror: function (err) {
              SyncFailures.note('CONN FAILED')
            },
            ontimeout: function () {
              SyncFailures.note('SYNC TIMEOUT')
            },
          })
        }
      } else if (typeof url === 'string' && url.includes('graphql')) {
        noteIgnoredOp(url)
      }
    }
    self.addEventListener('load', onLoad)
    return originalSend.apply(this, arguments)
  }

  UI.init()
  AutoSync.schedule()
})()
