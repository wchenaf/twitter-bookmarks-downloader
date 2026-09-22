// ==UserScript==
// @name         Twitter Bookmarks Sync to Local
// @namespace    http://tampermonkey.net/
// @version      0.6
// @description  Intercept XHR to sync bookmarks, with auto-scroll and unattended periodic sync.
// @author       Gemini
// @downloadURL  https://gist.githubusercontent.com/wchenaf/0e2d69dbb0f044f457f43baefdca202a/raw/sync-bookmarks.user.js
// @updateURL    https://gist.githubusercontent.com/wchenaf/0e2d69dbb0f044f457f43baefdca202a/raw/sync-bookmarks.user.js
// @homepageURL  https://gist.github.com/wchenaf/0e2d69dbb0f044f457f43baefdca202a
// @match        https://x.com/*
// @match        https://twitter.com/*
// @run-at       document-start
// @grant        unsafeWindow
// @grant        GM_xmlhttpRequest
// ==/UserScript==

;(function () {
  'use strict'
  // ⛔ 2026-09-23（H6）：TBD 守护（tbd.exe）已随 hgreport 从本机搬到 **NEIL-SERVER**
  //    ⇒ 原来那句 `http://localhost:41008/...`（跟着浏览器所在机器走）**指的是本机、那里已经没有它了**
  //    ⇒ 书签 URL 会**静默发不出去**（GM_xmlhttpRequest 失败只在控制台，页面上看不到）。
  //    ⇒ 改成 server 的 **LAN 地址**。⚠️ 两个前提：
  //      ① server 的防火墙必须放行 41008（只放 192.168.50.0/24，2026-09-23 已加）；
  //      ② 因此**离开家里 LAN 时抓取不工作**（旧写法"跟浏览器走"的那个能力**没有了** ——
  //         除非将来给 TBD 加自己的鉴权后经 nginx 暴露，那是另一件事）。
  const RAW_SYNC_URL = 'http://192.168.50.101:41008/api/sync-raw'

  // How often an idle bookmarks tab reloads itself to pick up new bookmarks.
  const AUTO_RELOAD_MS = 30 * 60 * 1000
  // When the tab is in the foreground a reload would yank the page out from
  // under whoever is reading it, so retry after this instead.
  const AUTO_RETRY_MS = 5 * 60 * 1000
  // Consecutive failures at which a run gives up.
  const MAX_SYNC_FAILURES = 5
  // Consecutive end-of-timeline pages at which a scroll is done. Past the last
  // bookmark x.com answers with a cursor-only page and would keep answering the
  // same way for every further request, so this is a real terminus rather than
  // a failure to retry: one page proves nothing (a hiccup mid-list looks the
  // same), three in a row does.
  const EMPTY_PAGE_LIMIT = 3
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

  console.log('[TBD v0.5] Terminal UI Edition loaded.')

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

  // RUN used to do nothing but walk the timeline downwards, which is the one
  // direction a new bookmark cannot be in: entries arrive at the top of the
  // timeline and scrolling only ever walks further back. So RUN reloads, and
  // the reload's own Bookmarks query is what brings the top page in. The scroll
  // then resumes on the far side of it, which keeps Force Mode's historical
  // sweep reachable from the same button instead of needing a second one.
  //
  // That intent has to outlive the page it was set on: whether to keep
  // scrolling is not known until the first batch has answered, and by then the
  // page that pressed RUN is gone. sessionStorage carries it across the reload,
  // and no further — an intent belongs to the tab that set it, and one that
  // survived into tomorrow's tab would start a scroll nobody asked for.
  const RESUME_SCROLL_KEY = 'tbd_resume_scroll'

  const RunIntent = {
    set() {
      try {
        sessionStorage.setItem(RESUME_SCROLL_KEY, '1')
      } catch (e) {
        // Storage unavailable (private mode, site data blocked). The reload
        // still syncs; all that is lost is the scroll that would follow it.
      }
    },

    take() {
      try {
        const set = sessionStorage.getItem(RESUME_SCROLL_KEY)
        if (set) sessionStorage.removeItem(RESUME_SCROLL_KEY)
        return !!set
      } catch (e) {
        return false
      }
    },
  }

  const Scroller = {
    active: false,
    timer: null,

    toggle() {
      if (this.active) {
        this.stop()
        return
      }
      // Off the bookmarks page a reload would land somewhere the backend never
      // hears from. The panel is hidden there, so this is a belt rather than a
      // path anyone walks.
      if (!onSyncPage()) return
      RunIntent.set()
      window.location.reload()
    },

    start() {
      if (this.active) return
      // A fresh run gets a fresh failure budget, so pressing RUN after fixing
      // the backend behaves like a first attempt rather than inheriting an
      // already-exhausted count. Same for the end-of-timeline counter.
      SyncFailures.reset()
      EmptyPages.reset()
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
      // @run-at document-start: a run resumed by RunIntent can tick before there
      // is a body to measure. Skipping the tick costs one interval; reading
      // scrollHeight off a null body would take the whole script down with it.
      if (document.body) window.scrollTo(0, document.body.scrollHeight)
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

  // Past the last bookmark the backend reports empty_page. That is a normal end
  // of run, not a failure, so it gets its own counter and its own label: fewer
  // requests than the failure path (three pages, not five) and END instead of
  // ABORTED. Green is deliberate — updateStatus holds anything green on screen,
  // so the panel keeps saying END until the next RUN rather than reverting to
  // the idle text after two seconds.
  const EmptyPages = {
    count: 0,

    reset() {
      this.count = 0
    },

    note() {
      this.count++
      if (this.count < EMPTY_PAGE_LIMIT) {
        UI.updateStatus(`END? ${this.count}/${EMPTY_PAGE_LIMIT}`, '#0ff')
        return
      }
      Scroller.stop()
      console.log(`[TBD] End of bookmark timeline (${this.count} empty pages).`)
      UI.updateStatus('END', '#0f0')
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
                if (res.empty_page) {
                  // Timeline entries came back and none of them was a tweet:
                  // the scroll has walked past the last bookmark. The backend
                  // only reports this when a cursor came with it, so a rate
                  // limit still lands in the NO DATA branch below.
                  EmptyPages.note()
                  SyncFailures.reset()
                } else {
                  // Any page that carried a tweet proves the list continues, so
                  // the end-of-timeline streak is over whichever branch we take.
                  EmptyPages.reset()
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
  // A RUN that asked to keep scrolling: the reload has landed, so honour it.
  // Deliberately unconditional — under Force Mode the top page may hold nothing
  // new while the historical sweep below it is the entire point, and with Force
  // off the first batch answers with the duplicate cutoff and stops it anyway.
  if (RunIntent.take()) Scroller.start()
  AutoSync.schedule()
})()
