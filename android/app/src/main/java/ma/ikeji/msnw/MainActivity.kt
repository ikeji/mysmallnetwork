package ma.ikeji.msnw

import android.Manifest
import android.content.ClipData
import android.content.ClipboardManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.LinearGradient
import android.graphics.Matrix
import android.graphics.Paint
import android.graphics.RectF
import android.graphics.Shader
import android.graphics.SweepGradient
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import android.os.SystemClock
import android.text.InputType
import android.text.SpannableStringBuilder
import android.text.Spanned
import android.text.style.RelativeSizeSpan
import android.util.Log
import android.util.TypedValue
import android.view.Gravity
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.View
import android.view.ViewGroup
import android.view.WindowManager
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputMethodManager
import android.webkit.HttpAuthHandler
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.webkit.WebViewDatabase
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.PopupMenu
import android.widget.PopupWindow
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.webkit.ProfileStore
import androidx.webkit.ProxyConfig
import androidx.webkit.ProxyController
import androidx.webkit.WebViewCompat
import androidx.webkit.WebViewFeature
import kotlin.math.abs
import kotlin.math.cos
import kotlin.math.sign
import kotlin.math.sin
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import com.termux.view.TerminalView
import com.termux.view.TerminalViewClient
import java.net.InetSocketAddress
import java.net.Socket
import java.nio.ByteBuffer
import java.nio.charset.Charset
import java.nio.charset.CodingErrorAction
import java.util.concurrent.Executors

/**
 * One activity with a strip of tabs. Browser tabs own a WebView (routed
 * through the local HTTP proxy) and their URLs are remembered across
 * restarts. Terminal tabs show sessions that belong to MsnwService, so they
 * survive this activity being destroyed; on start the tabs are rebuilt from
 * the service. Long-press a tab to close it.
 */
class MainActivity : AppCompatActivity() {
    private sealed class Tab(val button: Button) {
        class Browser(
            button: Button,
            val view: View,
            var web: WebView,
            val url: EditText,
            val willNavigate: () -> Unit,
            val reloadIfPending: () -> Unit,
            val resetProfile: () -> Unit
        ) : Tab(button)
        class Term(button: Button, val entry: MsnwService.Entry) : Tab(button)
    }

    private val tabs = mutableListOf<Tab>()
    private var current: Tab? = null
    internal var activeAuthDialog: AlertDialog? = null
    internal fun currentBrowserWebView(): WebView? = (current as? Tab.Browser)?.web

    private lateinit var strip: LinearLayout
    private lateinit var content: FrameLayout
    private lateinit var term: TerminalView
    private var ctrlPending = false
    private var altPending = false
    private var fontSize = 0

    private var svc: MsnwService? = null
    private var bound = false
    private val pending = mutableListOf<(MsnwService) -> Unit>()
    private var proxyReady = false
    private val onProxyReady = mutableListOf<() -> Unit>()
    private val connection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName, binder: IBinder) {
            val s = (binder as MsnwService.LocalBinder).service
            svc = s
            s.listener = sessionClient
            // Rebuild terminal tabs for sessions the service already has.
            synchronized(s.entries) { s.entries.toList() }.forEach { e ->
                if (tabs.none { it is Tab.Term && it.entry === e }) addTermTab(e)
            }
            pending.forEach { it(s) }
            pending.clear()
        }
        override fun onServiceDisconnected(name: ComponentName) { svc = null }
    }

    private fun bind() {
        if (!bound) {
            bindService(Intent(this, MsnwService::class.java), connection, Context.BIND_AUTO_CREATE)
            bound = true
        }
    }

    private fun withService(action: (MsnwService) -> Unit) {
        val s = svc
        if (s != null) action(s) else { pending += action; bind() }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        // Edge-to-edge (targetSdk 35): keep our views out from under the status
        // bar, the navigation bar and the on-screen keyboard.
        val root = findViewById<View>(R.id.root)
        ViewCompat.setOnApplyWindowInsetsListener(root) { v, insets ->
            val bars = insets.getInsets(WindowInsetsCompat.Type.systemBars() or WindowInsetsCompat.Type.displayCutout())
            val ime = insets.getInsets(WindowInsetsCompat.Type.ime())
            v.setPadding(bars.left, bars.top, bars.right, maxOf(bars.bottom, ime.bottom))
            WindowInsetsCompat.CONSUMED
        }
        Env.setup(this)
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        strip = findViewById(R.id.tabs)
        content = findViewById(R.id.content)
        cleanupOldWebProfiles()
        setupProxy()
        setupTerminal()
        setupSettings()
        setupSshKey()
        setupLauncher()
        setupVersion()
        findViewById<Button>(R.id.tabSettings).setOnClickListener { showSettings() }
        if (!Env.prefs(this).getString("key", "").isNullOrBlank()) {
            MsnwService.start(this)
            bind()
        }
        showSettings() // the "+" screen: open tabs from history or by typing
        if (savedInstanceState == null) handleIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleIntent(intent)
    }

    /** Rewrites an msnw:// URI to http://; returns null if not a valid msnw:// URI. */
    private fun msnwToHttp(uri: Uri?): String? {
        if (uri == null || !uri.scheme.equals("msnw", ignoreCase = true) || uri.host.isNullOrBlank()) return null
        return uri.buildUpon().scheme("http").build().toString()
    }

    private fun handleIntent(intent: Intent?) {
        if (intent?.action != Intent.ACTION_VIEW) return
        val httpUrl = msnwToHttp(intent.data) ?: return
        if (Env.prefs(this).getString("key", "").isNullOrBlank()) {
            toast("Set the link key first")
            showSettings()
            return
        }
        val open = {
            Thread {
                waitForProxy()
                runOnUiThread {
                    if (!isFinishing && !isDestroyed) select(newBrowserTab(httpUrl))
                }
            }.start()
        }
        if (proxyReady) open() else onProxyReady += open
    }

    /** Waits briefly for the local HTTP proxy to start listening (cold-start intents). */
    private fun waitForProxy(timeoutMs: Long = 3000L) {
        val deadline = SystemClock.uptimeMillis() + timeoutMs
        while (SystemClock.uptimeMillis() < deadline) {
            try {
                Socket().use { s ->
                    s.connect(InetSocketAddress("127.0.0.1", MsnwService.PROXY_PORT), 200)
                }
                return
            } catch (_: Exception) {
                try { Thread.sleep(50) } catch (_: InterruptedException) { return }
            }
        }
    }

    // ---- "+" screen: open web / mosh tabs ------------------------------------

    private fun setupLauncher() {
        val urlEdit = findViewById<EditText>(R.id.openUrl)
        val openUrl = {
            val u = urlEdit.text.toString().trim()
            if (u.isNotEmpty()) {
                urlEdit.setText("")
                val target = msnwToHttp(Uri.parse(u)) ?: if (u.contains("://")) u else "http://$u"
                select(newBrowserTab(target))
            }
        }
        findViewById<Button>(R.id.openUrlBtn).setOnClickListener { openUrl() }
        urlEdit.setOnEditorActionListener { _, id, _ -> if (id == EditorInfo.IME_ACTION_GO) { openUrl(); true } else false }

        val targetEdit = findViewById<EditText>(R.id.openTarget)
        val openTarget = {
            val t = targetEdit.text.toString().trim()
            if (t.isNotEmpty()) { targetEdit.setText(""); newTerminalTab(t) }
        }
        findViewById<Button>(R.id.openTargetBtn).setOnClickListener { openTarget() }
        targetEdit.setOnEditorActionListener { _, id, _ -> if (id == EditorInfo.IME_ACTION_GO) { openTarget(); true } else false }
    }

    /**
     * Fills a history list with one row per entry: tap opens, × removes it.
     * Only the first [HISTORY_SHOWN] entries are listed until "more" is tapped.
     */
    private fun fillHistory(container: LinearLayout, key: String, expanded: Boolean = false, open: (String) -> Unit) {
        container.removeAllViews()
        val all = Env.history(this, key)
        val shown = if (expanded) all else all.take(HISTORY_SHOWN)
        shown.forEach { item ->
            val row = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL }
            val label = Button(this, null, android.R.attr.borderlessButtonStyle).apply {
                text = item
                isAllCaps = false
                setTextColor(Color.WHITE)
                gravity = android.view.Gravity.START or android.view.Gravity.CENTER_VERTICAL
                setOnClickListener { open(item) }
            }
            val del = Button(this, null, android.R.attr.borderlessButtonStyle).apply {
                text = "×"
                setTextColor(Color.parseColor("#ff8080"))
                setOnClickListener { Env.removeHistory(this@MainActivity, key, item); fillHistory(container, key, expanded, open) }
            }
            row.addView(label, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
            row.addView(del, LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT))
            container.addView(row, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        }
        if (all.size > shown.size) {
            val more = Button(this, null, android.R.attr.borderlessButtonStyle).apply {
                text = "more (${all.size - shown.size})"
                isAllCaps = false
                setTextColor(Color.parseColor("#80c0ff"))
                setOnClickListener { fillHistory(container, key, true, open) }
            }
            container.addView(more, ViewGroup.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        }
    }

    private fun refreshHistories() {
        fillHistory(findViewById(R.id.urlHistory), URL_HISTORY) { select(newBrowserTab(it)) }
        fillHistory(findViewById(R.id.targetHistory), TARGET_HISTORY) { newTerminalTab(it) }
    }

    // ---- tab strip ----------------------------------------------------------

    private fun makeTabButton(label: String, tabRef: () -> Tab): Button {
        val b = Button(this, null, android.R.attr.borderlessButtonStyle)
        b.text = label
        b.setTextColor(Color.WHITE)
        b.setOnClickListener { select(tabRef()) }
        b.setOnLongClickListener { closeTab(tabRef()); true }
        strip.addView(b)
        return b
    }

    private fun select(tab: Tab) {
        current = tab
        findViewById<View>(R.id.settings).visibility = View.GONE
        findViewById<View>(R.id.terminal).visibility = if (tab is Tab.Term) View.VISIBLE else View.GONE
        tabs.forEach {
            it.button.setTextColor(if (it === tab) Color.parseColor("#7fd1ff") else Color.WHITE)
            if (it is Tab.Browser) it.view.visibility = if (it === tab) View.VISIBLE else View.GONE
        }
        if (tab is Tab.Browser) {
            tab.reloadIfPending()
        } else if (tab is Tab.Term) {
            term.attachSession(tab.entry.session)
            showKeyboard()
        }
    }

    private fun closeTab(tab: Tab) {
        when (tab) {
            is Tab.Browser -> { content.removeView(tab.view); tab.web.destroy() }
            is Tab.Term -> withService { it.close(tab.entry) }
        }
        strip.removeView(tab.button)
        val idx = tabs.indexOf(tab)
        tabs.remove(tab)
        if (current === tab) {
            if (tabs.isEmpty()) showSettings() else select(tabs[minOf(idx, tabs.size - 1)])
        }
    }

    private fun showSettings() {
        current = null
        findViewById<View>(R.id.terminal).visibility = View.GONE
        tabs.forEach { it.button.setTextColor(Color.WHITE); if (it is Tab.Browser) it.view.visibility = View.GONE }
        findViewById<View>(R.id.settings).visibility = View.VISIBLE
        refreshHistories()
        refreshLog()
    }

    // ---- browser tabs -------------------------------------------------------

    private fun setupProxy() {
        if (WebViewFeature.isFeatureSupported(WebViewFeature.PROXY_OVERRIDE) &&
            WebViewFeature.isFeatureSupported(WebViewFeature.PROXY_OVERRIDE_REVERSE_BYPASS)) {
            // Route only msnw names (bare hostnames and *.msnw) through the proxy;
            // everything else goes directly so normal web URLs use Android's DNS.
            val config = ProxyConfig.Builder()
                .addProxyRule("http://127.0.0.1:${MsnwService.PROXY_PORT}")
                .bypassSimpleHostnames()
                .addBypassRule("*.msnw")
                .setReverseBypassEnabled(true)
                .build()
            ProxyController.getInstance().setProxyOverride(config, Executors.newSingleThreadExecutor()) {
                Log.i(MsnwService.TAG, "webview proxy set")
                runOnUiThread {
                    proxyReady = true
                    onProxyReady.forEach { it() }
                    onProxyReady.clear()
                }
            }
        } else {
            proxyReady = true
            toast("This WebView cannot use a proxy; update Android System WebView")
        }
    }

    /** Tab label for a browser tab before the page sends a title: the URL's host, else "B". */
    private fun webLabel(u: String?): String =
        u?.let { Uri.parse(it).host }?.takeIf { it.isNotBlank() }?.take(12) ?: "B"

    /**
     * Flashy loading overlay for a browser tab: a flowing neon rainbow progress bar
     * along the top edge plus a centered HUD card with counter-rotating neon rings.
     * Non-clickable so touches pass straight through to the WebView underneath.
     */
    private inner class BrowserLoader : View(this@MainActivity) {
        private val d = resources.displayMetrics.density
        private val matrix = Matrix()
        private val rect = RectF()
        private val trackPaint = Paint().apply { color = 0xbb111111.toInt() }
        private val barPaint = Paint(Paint.ANTI_ALIAS_FLAG)
        private val glowPaint = Paint(Paint.ANTI_ALIAS_FLAG)
        private val sparkPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply { color = Color.WHITE }
        private val cardPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply { color = 0xe010141f.toInt() }
        private val haloPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply { style = Paint.Style.STROKE; strokeWidth = 7 * d }
        private val borderPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply { style = Paint.Style.STROKE; strokeWidth = 2.5f * d }
        private val arcPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply { style = Paint.Style.STROKE; strokeCap = Paint.Cap.ROUND }
        private val corePaint = Paint(Paint.ANTI_ALIAS_FLAG).apply { color = 0xff00e676.toInt() }
        private val textPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
            color = Color.WHITE
            textAlign = Paint.Align.CENTER
            textSize = TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_SP, 14f, resources.displayMetrics)
            typeface = Typeface.DEFAULT_BOLD
        }
        private var barShader: LinearGradient? = null
        private val sweepShader = SweepGradient(
            0f, 0f,
            intArrayOf(0xff00e5ff.toInt(), 0xffff4081.toInt(), 0xffffea00.toInt(), 0xff00e676.toInt(), 0xff00e5ff.toInt()),
            null
        )

        var active = false
            private set
        var progress = 0
            private set
        private var shown = 0f

        init {
            visibility = GONE
            borderPaint.shader = sweepShader
            haloPaint.shader = sweepShader
            haloPaint.alpha = 80
        }

        override fun onSizeChanged(w: Int, h: Int, oldw: Int, oldh: Int) {
            if (w > 0) {
                barShader = LinearGradient(
                    0f, 0f, w.toFloat(), 0f,
                    intArrayOf(
                        0xff00e5ff.toInt(), 0xff00e676.toInt(), 0xffffea00.toInt(),
                        0xffff4081.toInt(), 0xffd500f9.toInt(), 0xff00e5ff.toInt()
                    ),
                    null, Shader.TileMode.REPEAT
                )
                barPaint.shader = barShader
                glowPaint.shader = barShader
                glowPaint.alpha = 95
            }
        }

        fun start(p: Int) {
            if (!active) {
                active = true
                shown = p.toFloat()
                animate().cancel()
                alpha = 1f
                visibility = VISIBLE
            }
            progress = p
            invalidate()
        }

        fun finish(immediate: Boolean = false) {
            if (!active) return
            active = false
            progress = 100
            if (immediate) {
                animate().cancel()
                visibility = GONE
                shown = 0f
            } else {
                shown = 100f
                invalidate()
                animate().alpha(0f).setDuration(250).withEndAction {
                    if (!active) { visibility = GONE; shown = 0f }
                }.start()
            }
        }

        override fun onDraw(canvas: Canvas) {
            val w = width.toFloat()
            val h = height.toFloat()
            if (w <= 0f || h <= 0f) return
            val t = SystemClock.uptimeMillis() / 1000f

            val target = if (active) maxOf(progress.toFloat(), 12f) else 100f
            shown += (target - shown) * 0.2f

            val barH = 7 * d
            val fillW = (w * (shown / 100f)).coerceIn(0f, w)
            canvas.drawRect(0f, 0f, w, barH, trackPaint)
            barShader?.let {
                matrix.setTranslate((t * w * 0.9f) % w, 0f)
                it.setLocalMatrix(matrix)
            }
            canvas.drawRect(0f, barH, fillW, barH + 14 * d, glowPaint)
            canvas.drawRect(0f, 0f, fillW, barH, barPaint)
            val pulse = 0.7f + 0.3f * sin(t * 10f)
            sparkPaint.alpha = (220 * pulse).toInt()
            canvas.drawCircle(fillW, barH / 2f, 5 * d * pulse, sparkPaint)

            if (h >= 120 * d) {
                val cx = w / 2f
                val cy = h / 2f
                val cw = 172 * d
                val ch = 148 * d
                val cr = 22 * d
                rect.set(cx - cw / 2f, cy - ch / 2f, cx + cw / 2f, cy + ch / 2f)
                matrix.setRotate((t * 180f) % 360f)
                matrix.postTranslate(cx, cy)
                sweepShader.setLocalMatrix(matrix)
                canvas.drawRoundRect(rect, cr, cr, cardPaint)
                canvas.drawRoundRect(rect, cr, cr, haloPaint)
                canvas.drawRoundRect(rect, cr, cr, borderPaint)

                val ry = cy - 16 * d
                fun arc(radius: Float, stroke: Float, color: Int, start: Float, sweep: Float) {
                    arcPaint.strokeWidth = stroke
                    arcPaint.color = color
                    rect.set(cx - radius, ry - radius, cx + radius, ry + radius)
                    canvas.drawArc(rect, start, sweep, false, arcPaint)
                }
                arc(33 * d, 4.5f * d, 0xff00e5ff.toInt(), (t * 240f) % 360f, 210f + 40f * sin(t * 4f))
                arc(24 * d, 4f * d, 0xffff4081.toInt(), (-t * 330f) % 360f, 170f + 40f * cos(t * 5f))
                arc(15 * d, 3.5f * d, 0xffffea00.toInt(), (t * 450f) % 360f, 120f)
                canvas.drawCircle(cx, ry, (4.5f + 1.5f * sin(t * 8f)) * d, corePaint)

                val label = if (progress < 15 && active) "CONNECTING..." else "LOADING ${progress}%"
                canvas.drawText(label, cx, cy + 48 * d, textPaint)
            }

            if (visibility == VISIBLE) postInvalidateOnAnimation()
        }
    }

    private fun newBrowserTab(initialUrl: String): Tab.Browser {
        val column = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        val bar = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL; setBackgroundColor(Color.parseColor("#333333")) }
        val url = EditText(this).apply {
            inputType = InputType.TYPE_TEXT_VARIATION_URI
            imeOptions = EditorInfo.IME_ACTION_GO
            isSingleLine = true
            hint = "http://mypc/"
            setTextColor(Color.WHITE); setHintTextColor(Color.GRAY)
        }
        val go = Button(this).apply { text = "Go" }
        val reload = Button(this).apply { text = "↻" }
        val reloadIdleColor = reload.currentTextColor
        val star = Button(this).apply { text = "☆" } // bookmark: add the current page to the list
        bar.addView(url, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
        bar.addView(go)
        bar.addView(reload)
        bar.addView(star)

        val loader = BrowserLoader()
        val webBox = FrameLayout(this)
        var pageLabel = webLabel(initialUrl)
        var stopping = false
        var pendingReload = false
        lateinit var web: WebView
        lateinit var tab: Tab.Browser
        val button = makeTabButton(pageLabel) { tab }
        fun syncUi() {
            button.text = if (loader.active) "⏳ $pageLabel" else pageLabel
            reload.text = if (loader.active) "✕" else "↻"
            reload.setTextColor(if (loader.active) Color.parseColor("#ff6b81") else reloadIdleColor)
        }
        fun setLoading(loading: Boolean, progress: Int = 0, immediate: Boolean = false) {
            if (loading) loader.start(progress) else loader.finish(immediate)
            syncUi()
        }
        val webClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView?, request: WebResourceRequest?): Boolean {
                if (view !== web) return false
                val httpUrl = msnwToHttp(request?.url) ?: return false
                url.setText(httpUrl)
                stopping = false
                setLoading(true, 5)
                web.loadUrl(httpUrl)
                return true
            }
            // Label: the host name until the page reports a title (onReceivedTitle below).
            override fun onPageStarted(view: WebView?, u: String?, favicon: Bitmap?) {
                if (view !== web) return
                stopping = false
                pageLabel = webLabel(u)
                setLoading(true, if (loader.active) maxOf(loader.progress, 10) else 10)
            }
            override fun onPageFinished(view: WebView?, u: String?) {
                if (view !== web) return
                url.setText(u ?: "")
                stopping = false
                setLoading(false)
            }
            override fun onReceivedHttpAuthRequest(view: WebView?, handler: HttpAuthHandler?, host: String?, realm: String?) {
                if (view !== web || handler == null || host == null) {
                    handler?.cancel()
                    return
                }
                val wasLoading = loader.active
                promptHttpAuth(
                    handler, host, realm,
                    onDialogShown = { if (wasLoading) setLoading(false, immediate = true) },
                    onProceed = { if (wasLoading && !stopping) setLoading(true, 15) }
                )
            }
        }
        val chromeClient = object : WebChromeClient() {
            override fun onReceivedTitle(view: WebView?, title: String?) {
                if (view !== web) return
                if (!title.isNullOrBlank()) {
                    pageLabel = title.take(12)
                    syncUi()
                }
            }
            override fun onProgressChanged(view: WebView?, newProgress: Int) {
                if (view !== web) return
                if (newProgress >= 100) {
                    stopping = false
                    setLoading(false)
                } else if (!stopping) {
                    setLoading(true, newProgress)
                }
            }
        }
        web = buildWebView(webClient, chromeClient)
        webBox.addView(web, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
        webBox.addView(loader, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
        column.addView(bar, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        column.addView(webBox, LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f))
        content.addView(column, 0, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))

        val navigate = {
            var u = url.text.toString().trim()
            if (u.isNotEmpty()) {
                u = msnwToHttp(Uri.parse(u)) ?: if (u.contains("://")) u else "http://$u"
                Env.addHistory(this, URL_HISTORY, u)
                pendingReload = false
                stopping = false
                setLoading(true, 5)
                web.loadUrl(u)
                (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).hideSoftInputFromWindow(url.windowToken, 0)
            }
        }
        val resetProfile = {
            val prevUrl = web.url?.takeIf { it != "about:blank" } ?: url.text.toString().trim()
            stopping = true
            setLoading(false, immediate = true)
            val old = web
            webBox.removeView(old)
            old.stopLoading()
            old.destroy()
            web = buildWebView(webClient, chromeClient)
            tab.web = web
            webBox.addView(web, 0, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
            url.setText(prevUrl)
            pendingReload = prevUrl.isNotBlank()
        }
        tab = Tab.Browser(
            button, column, web, url,
            willNavigate = { stopping = false; setLoading(true, 5) },
            reloadIfPending = { if (pendingReload) navigate() },
            resetProfile = resetProfile
        )
        tabs.add(tab)
        go.setOnClickListener { navigate() }
        reload.setOnClickListener {
            if (loader.active) {
                stopping = true
                web.stopLoading()
                setLoading(false, immediate = true)
            } else if (pendingReload || web.url.isNullOrBlank()) {
                navigate()
            } else {
                stopping = false
                setLoading(true, 5)
                web.reload()
            }
        }
        star.setOnClickListener {
            val u = web.url ?: ""
            if (u.isNotBlank() && u != "about:blank") { Env.addHistory(this, URL_HISTORY, u); toast("bookmarked") }
        }
        url.setOnEditorActionListener { _, id, _ -> if (id == EditorInfo.IME_ACTION_GO) { navigate(); true } else false }
        if (initialUrl.isNotBlank()) { url.setText(initialUrl); navigate() }
        return tab
    }

    private fun currentWebProfile(): String =
        "msnw_${Env.prefs(this).getInt("web_profile_seq", 1)}"

    private fun buildWebView(client: WebViewClient, chrome: WebChromeClient): WebView {
        val web = WebView(this)
        if (WebViewFeature.isFeatureSupported(WebViewFeature.MULTI_PROFILE)) {
            WebViewCompat.setProfile(web, currentWebProfile())
        }
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        web.settings.useWideViewPort = true
        web.settings.loadWithOverviewMode = true
        web.webViewClient = client
        web.webChromeClient = chrome
        return web
    }

    private fun cleanupOldWebProfiles() {
        if (!WebViewFeature.isFeatureSupported(WebViewFeature.MULTI_PROFILE)) return
        val active = currentWebProfile()
        val store = ProfileStore.getInstance()
        store.allProfileNames.filter { it.startsWith("msnw_") && it != active }.forEach {
            try { store.deleteProfile(it) } catch (_: Exception) {}
        }
    }

    private fun httpAuthPrefs() = getSharedPreferences("msnw_http_auth", Context.MODE_PRIVATE)

    /**
     * Handles HTTP Basic/Digest auth challenges for WebView (whose default is
     * handler.cancel()). Reuses stored credentials on the first attempt for a
     * request and falls back to a username/password dialog if none are saved or
     * the server rejected them.
     */
    private fun promptHttpAuth(
        handler: HttpAuthHandler,
        host: String,
        realm: String?,
        onDialogShown: () -> Unit,
        onProceed: () -> Unit
    ) {
        val prefs = httpAuthPrefs()
        val r = realm ?: ""
        val uk = "u:$host\n$r"
        val pk = "p:$host\n$r"
        val savedUser = prefs.getString(uk, null)
        val savedPass = prefs.getString(pk, null)
        if (handler.useHttpAuthUsernamePassword() && prefs.contains(uk) && savedUser != null && savedPass != null) {
            handler.proceed(savedUser, savedPass)
            return
        }
        if (isFinishing || isDestroyed) {
            handler.cancel()
            return
        }
        onDialogShown()
        val pad = (20 * resources.displayMetrics.density).toInt()
        val user = EditText(this).apply {
            hint = "Username"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            imeOptions = EditorInfo.IME_ACTION_NEXT
            isSingleLine = true
            if (!savedUser.isNullOrEmpty()) setText(savedUser)
        }
        val pass = EditText(this).apply {
            hint = "Password"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            imeOptions = EditorInfo.IME_ACTION_DONE
            isSingleLine = true
        }
        val form = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad / 2, pad, 0)
            addView(user)
            addView(pass)
        }
        var handled = false
        fun submit(dialog: AlertDialog) {
            if (handled) return
            handled = true
            val u = user.text.toString()
            val p = pass.text.toString()
            prefs.edit().putString(uk, u).putString(pk, p).apply()
            onProceed()
            handler.proceed(u, p)
            dialog.dismiss()
        }
        val displayRealm = decodeRealm(r)
        val dialog = AlertDialog.Builder(this)
            .setTitle("Sign in")
            .setMessage(if (displayRealm.isBlank()) host else "$host ($displayRealm)")
            .setView(form)
            .setPositiveButton("Sign in") { d, _ -> submit(d as AlertDialog) }
            .setNegativeButton("Cancel") { _, _ -> handled = true; handler.cancel() }
            .setOnDismissListener {
                if (activeAuthDialog === it) activeAuthDialog = null
                if (!handled) { handled = true; handler.cancel() }
            }
            .create()
        pass.setOnEditorActionListener { _, id, _ ->
            if (id == EditorInfo.IME_ACTION_DONE) { submit(dialog); true } else false
        }
        dialog.window?.setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_STATE_VISIBLE)
        activeAuthDialog = dialog
        dialog.show()
        (if (user.text.isNullOrEmpty()) user else pass).requestFocus()
    }

    /**
     * Chromium decodes unlabelled WWW-Authenticate realm bytes as windows-1252
     * (mapping 0x80..0x9F to punctuation like U+2020 '†' or U+201D '”' and
     * leaving 0x81/0x8D/0x8F/0x90/0x9D as C1 controls). Recover all 0x00..0xFF
     * bytes losslessly and decode as UTF-8 (or Shift_JIS / EUC-JP).
     */
    private fun decodeRealm(s: String): String {
        if (s.all { it.code < 0x80 }) return s
        val raw = ByteArray(s.length)
        for (i in s.indices) {
            val b = cp1252Byte(s[i].code) ?: return s
            raw[i] = b
        }
        for (name in listOf("UTF-8", "Windows-31J", "EUC-JP")) {
            try {
                return Charset.forName(name).newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(raw))
                    .toString()
            } catch (_: Exception) {
            }
        }
        return s
    }

    private fun cp1252Byte(code: Int): Byte? = when (code) {
        in 0x00..0xFF -> code.toByte()
        0x20AC -> 0x80.toByte()
        0x201A -> 0x82.toByte()
        0x0192 -> 0x83.toByte()
        0x201E -> 0x84.toByte()
        0x2026 -> 0x85.toByte()
        0x2020 -> 0x86.toByte()
        0x2021 -> 0x87.toByte()
        0x02C6 -> 0x88.toByte()
        0x2030 -> 0x89.toByte()
        0x0160 -> 0x8A.toByte()
        0x2039 -> 0x8B.toByte()
        0x0152 -> 0x8C.toByte()
        0x017D -> 0x8E.toByte()
        0x2018 -> 0x91.toByte()
        0x2019 -> 0x92.toByte()
        0x201C -> 0x93.toByte()
        0x201D -> 0x94.toByte()
        0x2022 -> 0x95.toByte()
        0x2013 -> 0x96.toByte()
        0x2014 -> 0x97.toByte()
        0x02DC -> 0x98.toByte()
        0x2122 -> 0x99.toByte()
        0x0161 -> 0x9A.toByte()
        0x203A -> 0x9B.toByte()
        0x0153 -> 0x9C.toByte()
        0x017E -> 0x9E.toByte()
        0x0178 -> 0x9F.toByte()
        else -> null
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        val c = current
        if (c is Tab.Browser && c.web.canGoBack()) { c.willNavigate(); c.web.goBack() }
        else @Suppress("DEPRECATION") super.onBackPressed()
    }

    // ---- terminal tabs ------------------------------------------------------

    private fun setupTerminal() {
        term = findViewById(R.id.term)
        fontSize = (14 * resources.displayMetrics.density).toInt()
        term.setTextSize(fontSize)
        term.setTerminalViewClient(viewClient)
        term.keepScreenOn = true
        val send = { s: String -> if (s.isNotEmpty()) (current as? Tab.Term)?.entry?.session?.write(s) }
        // Ctrl and Alt apply to the next key typed, like sticky modifiers.
        val toggleCtrl = { ctrlPending = !ctrlPending; toast(if (ctrlPending) "Ctrl on" else "Ctrl off") }
        val toggleAlt = { altPending = !altPending; toast(if (altPending) "Alt on" else "Alt off") }
        // Esc is tapped all the time (vi), so it stays one tap; Tab, Ctrl and Alt ride on the same key as flicks.
        flickKey(findViewById(R.id.kEsc), up = Flick("Tab") { send("\t") }, down = Flick("Ctrl", toggleCtrl), right = Flick("Alt", toggleAlt),
            tap = Flick("Esc") { send("\u001b") }, repeat = false)
        val arrows = Flick.arrows(send, "\u001b[A", "\u001b[B", "\u001b[D", "\u001b[C")
        flickKey(findViewById(R.id.kArrows), arrows[0], arrows[1], arrows[2], arrows[3])
        flickKey(findViewById(R.id.kPage), up = Flick("PgUp") { send("\u001b[5~") }, down = Flick("PgDn") { send("\u001b[6~") },
            left = Flick("Home") { send("\u001b[H") }, right = Flick("End") { send("\u001b[F") })
        // Text input row for IMEs that cannot type into the terminal view directly (Japanese etc.).
        val input = findViewById<EditText>(R.id.termInput)
        val sendInput = { _: Boolean ->
            send(input.text.toString() + "\r") // a line, like typing it and pressing Enter
            input.setText("")
        }
        val inputRow = findViewById<View>(R.id.termInputRow)
        val keys = findViewById<View>(R.id.keys)
        // The input row takes the place of the key row; its × brings the keys back.
        val toggleInputRow: () -> Unit = {
            val show = inputRow.visibility != View.VISIBLE
            inputRow.visibility = if (show) View.VISIBLE else View.GONE
            keys.visibility = if (show) View.GONE else View.VISIBLE
            if (show) {
                input.requestFocus()
                (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).showSoftInput(input, InputMethodManager.SHOW_IMPLICIT)
            } else {
                showKeyboard()
            }
        }
        // Shell punctuation that is a chore to reach on a phone keyboard rides on the あ key.
        flickKey(findViewById(R.id.kText), up = Flick(":") { send(":") }, down = Flick("$") { send("$") },
            left = Flick("-") { send("-") }, right = Flick("/") { send("/") }, tap = Flick("あ", toggleInputRow), repeat = false)
        findViewById<Button>(R.id.kInputClose).setOnClickListener { toggleInputRow() }
        input.setOnEditorActionListener { _, id, _ -> if (id == EditorInfo.IME_ACTION_SEND) { sendInput(true); true } else false }
        input.setOnKeyListener { _, keyCode, event ->
            if (keyCode == KeyEvent.KEYCODE_ENTER && event.action == KeyEvent.ACTION_DOWN) { sendInput(true); true } else false
        }
        // Rarely used actions live in a menu so the key row fits a phone screen without scrolling.
        val more = findViewById<Button>(R.id.kMore)
        more.setOnClickListener {
            PopupMenu(this, more).apply {
                menu.add("Reconnect").setOnMenuItemClickListener { (current as? Tab.Term)?.let { restartSession(it) }; true }
                menu.add("Close tab").setOnMenuItemClickListener { (current as? Tab.Term)?.let { closeTab(it) }; true }
            }.show()
        }
    }

    /** One direction of a flick key: [name] is shown in the guide, [act] is what it does. */
    private class Flick(val name: String, val act: () -> Unit) {
        companion object {
            fun arrows(send: (String) -> Unit, up: String, down: String, left: String, right: String) =
                listOf(Flick("↑") { send(up) }, Flick("↓") { send(down) }, Flick("←") { send(left) }, Flick("→") { send(right) })
        }
    }

    /**
     * The guide shown above a flick key while it is held: a cross with the action of each
     * direction, the tap action in the middle. [hit] lights up a direction.
     */
    private inner class FlickGuide(up: Flick?, down: Flick?, left: Flick?, right: Flick?, private val tap: String) {
        private val d = resources.displayMetrics.density
        private val cells = HashMap<Flick, TextView>()
        private val popup: PopupWindow
        private var lit: TextView? = null

        init {
            fun cell(f: Flick?): TextView = TextView(this@MainActivity).apply {
                text = f?.name ?: ""
                setTextColor(Color.WHITE); textSize = 14f
                gravity = Gravity.CENTER
                layoutParams = LinearLayout.LayoutParams((56 * d).toInt(), (36 * d).toInt())
                if (f != null) cells[f] = this
            }
            fun row(vararg v: View) = LinearLayout(this@MainActivity).apply {
                orientation = LinearLayout.HORIZONTAL
                v.forEach { addView(it) }
            }
            val center = cell(null).apply { text = tap; setTextColor(Color.parseColor("#7fd1ff")) }
            val box = LinearLayout(this@MainActivity).apply {
                orientation = LinearLayout.VERTICAL
                background = GradientDrawable().apply { setColor(0xee333333.toInt()); cornerRadius = 8 * d }
                addView(row(cell(null), cell(up), cell(null)))
                addView(row(cell(left), center, cell(right)))
                addView(row(cell(null), cell(down), cell(null)))
            }
            box.measure(View.MeasureSpec.UNSPECIFIED, View.MeasureSpec.UNSPECIFIED)
            popup = PopupWindow(box, box.measuredWidth, box.measuredHeight).apply { isTouchable = false }
        }

        fun show(anchor: View) {
            lit?.setBackgroundColor(Color.TRANSPARENT); lit = null
            val loc = IntArray(2).also { anchor.getLocationInWindow(it) }
            val x = loc[0] + anchor.width / 2 - popup.width / 2
            val y = loc[1] - popup.height - (8 * d).toInt()
            popup.showAtLocation(anchor, Gravity.NO_GRAVITY, x, y)
        }

        fun hit(f: Flick) {
            if (lit !== cells[f]) { lit?.setBackgroundColor(Color.TRANSPARENT); lit = cells[f]; lit?.setBackgroundColor(0xff1565c0.toInt()) }
        }

        fun hide() = popup.dismiss()
    }

    /**
     * The face of a flick key: [main] in the middle, the flick directions around it in small
     * type, so the key itself says what a flick does.
     *
     *       Tab
     *   ←   Esc   →
     *       Ctrl
     */
    private fun flickLabel(main: CharSequence, up: Flick?, down: Flick?, left: Flick?, right: Flick?): CharSequence {
        val b = SpannableStringBuilder()
        fun small(t: String) {
            val from = b.length
            b.append(t)
            b.setSpan(RelativeSizeSpan(0.6f), from, b.length, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        }
        small(up?.name ?: " "); b.append('\n')
        small(left?.name ?: ""); if (left != null) b.append(' ')
        b.append(main)
        if (right != null) b.append(' '); small(right?.name ?: "")
        b.append('\n'); small(down?.name ?: " ")
        return b
    }

    /**
     * One key for up to five actions: a flick of [FLICK_STEP_DP] runs the action of its
     * direction, a plain tap runs [tap] (or, when null, repeats the last flicked direction).
     * With [repeat], dragging on runs the direction again every key width (or height) so the
     * cursor follows the finger. A guide with the assignments is shown while the key is held.
     */
    private fun flickKey(button: Button, up: Flick? = null, down: Flick? = null, left: Flick? = null, right: Flick? = null,
                         tap: Flick? = null, repeat: Boolean = true) {
        val flick = FLICK_STEP_DP * resources.displayMetrics.density
        val guide = FlickGuide(up, down, left, right, tap?.name ?: "again")
        button.text = flickLabel(button.text, up, down, left, right)
        button.isAllCaps = false // the framework's all-caps transform would drop the label's spans
        button.includeFontPadding = false
        button.minHeight = 0; button.minimumHeight = 0 // as tall as the three lines need, no more
        button.setPadding(0, button.paddingTop / 2, 0, button.paddingBottom / 2)
        var ox = 0f; var oy = 0f // where the next step is measured from
        var count = 0 // keys sent by this touch
        var last: Flick? = null
        fun fire(f: Flick) { f.act(); count++; last = f; guide.hit(f) }
        fun release(v: View) { v.isPressed = false; guide.hide() }
        button.setOnTouchListener { v, e ->
            when (e.actionMasked) {
                MotionEvent.ACTION_DOWN -> {
                    ox = e.x; oy = e.y; count = 0; v.isPressed = true
                    guide.show(v)
                }
                MotionEvent.ACTION_MOVE -> if (repeat || count == 0) {
                    val dx = e.x - ox; val dy = e.y - oy
                    val sx = if (count == 0) flick else v.width.toFloat() // the first key comes from a flick, the rest per key size
                    val sy = if (count == 0) flick else v.height.toFloat()
                    if (abs(dx) >= sx && abs(dx) >= abs(dy)) {
                        ox += sx * sign(dx); oy = e.y
                        (if (dx > 0) right else left)?.let { fire(it) }
                    } else if (abs(dy) >= sy) {
                        oy += sy * sign(dy); ox = e.x
                        (if (dy > 0) down else up)?.let { fire(it) }
                    }
                }
                MotionEvent.ACTION_UP -> {
                    release(v)
                    if (count == 0) (tap ?: last)?.act()
                    v.performClick()
                }
                MotionEvent.ACTION_CANCEL -> release(v)
            }
            true
        }
    }

    private fun newTerminalTab(target: String) {
        if (Env.prefs(this).getString("key", "").isNullOrBlank()) {
            toast("Set the link key first")
            return
        }
        Env.addHistory(this, TARGET_HISTORY, target)
        withService { s ->
            Thread {
                val env = Env.envArray(this) // resolves the server name: off the UI thread
                runOnUiThread { select(addTermTab(s.newSession(target, env))) } // TerminalSession needs the main thread
            }.start()
        }
    }

    /** Tab label: the title set by the shell (OSC 0/2), else the target's host name; "!" marks a finished session. */
    private fun termLabel(tab: Tab.Term): String {
        val title = (tab.entry.session.title?.takeIf { it.isNotBlank() } ?: hostOf(tab.entry.target)).take(16)
        return if (tab.entry.finished) "$title!" else title
    }

    /** "user@host" -> "host"; a bare host name is returned as is. */
    private fun hostOf(target: String): String = target.substringAfterLast('@').ifBlank { target }

    private fun addTermTab(entry: MsnwService.Entry): Tab.Term {
        lateinit var tab: Tab.Term
        val button = makeTabButton("") { tab }
        tab = Tab.Term(button, entry)
        button.text = termLabel(tab)
        tabs.add(tab)
        return tab
    }

    private fun restartSession(tab: Tab.Term) {
        Env.addHistory(this, TARGET_HISTORY, tab.entry.target)
        withService { s ->
            Thread {
                val env = Env.envArray(this)
                runOnUiThread {
                    s.restart(tab.entry, env)
                    tab.button.text = termLabel(tab)
                    if (current === tab) { term.attachSession(tab.entry.session); showKeyboard() }
                }
            }.start()
        }
    }

    private fun toast(s: String) = Toast.makeText(this, s, Toast.LENGTH_SHORT).show()

    private fun showKeyboard() {
        term.post {
            term.requestFocus()
            WindowInsetsControllerCompat(window, term).show(WindowInsetsCompat.Type.ime())
            (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).showSoftInput(term, InputMethodManager.SHOW_IMPLICIT)
        }
    }

    private fun termTabOf(session: TerminalSession): Tab.Term? =
        tabs.filterIsInstance<Tab.Term>().firstOrNull { it.entry.session === session }

    private val sessionClient = object : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) {
            if ((current as? Tab.Term)?.entry?.session === changedSession) term.onScreenUpdated()
        }
        override fun onTitleChanged(changedSession: TerminalSession) {
            termTabOf(changedSession)?.let { it.button.text = termLabel(it) }
        }
        override fun onSessionFinished(finishedSession: TerminalSession) {
            runOnUiThread {
                termTabOf(finishedSession)?.let { it.button.text = termLabel(it) }
                if ((current as? Tab.Term)?.entry?.session === finishedSession) toast("session ended (exit ${finishedSession.exitStatus}); Reconnect or close the tab")
            }
        }
        override fun onCopyTextToClipboard(session: TerminalSession, text: String) {
            (getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).setPrimaryClip(ClipData.newPlainText("msnw", text))
        }
        override fun onPasteTextFromClipboard(session: TerminalSession?) {
            val clip = (getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).primaryClip ?: return
            if (clip.itemCount > 0) session?.write(clip.getItemAt(0).coerceToText(this@MainActivity).toString())
        }
        override fun onBell(session: TerminalSession) {}
        override fun onColorsChanged(session: TerminalSession) { term.onScreenUpdated() }
        override fun onTerminalCursorStateChange(state: Boolean) {}
        override fun setTerminalShellPid(session: TerminalSession, pid: Int) {}
        override fun getTerminalCursorStyle(): Int? = null
        override fun logError(tag: String, message: String) { Log.e(tag, message) }
        override fun logWarn(tag: String, message: String) { Log.w(tag, message) }
        override fun logInfo(tag: String, message: String) { Log.i(tag, message) }
        override fun logDebug(tag: String, message: String) { Log.d(tag, message) }
        override fun logVerbose(tag: String, message: String) { Log.v(tag, message) }
        override fun logStackTraceWithMessage(tag: String, message: String, e: Exception) { Log.e(tag, message, e) }
        override fun logStackTrace(tag: String, e: Exception) { Log.e(tag, "", e) }
    }

    private val viewClient = object : TerminalViewClient {
        override fun onScale(scale: Float): Float {
            if (scale < 0.9f || scale > 1.1f) {
                fontSize = (fontSize * scale).toInt().coerceIn(20, 80)
                term.setTextSize(fontSize)
                return 1.0f
            }
            return scale
        }
        override fun onSingleTapUp(e: MotionEvent) { showKeyboard() }
        override fun shouldBackButtonBeMappedToEscape() = false
        override fun shouldEnforceCharBasedInput() = false // let IMEs compose (Japanese input)
        override fun shouldUseCtrlSpaceWorkaround() = false
        override fun isTerminalViewSelected() = true
        override fun copyModeChanged(copyMode: Boolean) {}
        override fun onKeyDown(keyCode: Int, e: KeyEvent, session: TerminalSession): Boolean = false
        override fun onKeyUp(keyCode: Int, e: KeyEvent): Boolean = false
        override fun onLongPress(event: MotionEvent): Boolean = false
        override fun readControlKey(): Boolean { val c = ctrlPending; ctrlPending = false; return c }
        override fun readAltKey(): Boolean { val a = altPending; altPending = false; return a }
        override fun readShiftKey() = false
        override fun readFnKey() = false
        override fun onCodePoint(codePoint: Int, ctrlDown: Boolean, session: TerminalSession): Boolean = false
        override fun onEmulatorSet() { term.updateSize() }
        override fun logError(tag: String, message: String) { Log.e(tag, message) }
        override fun logWarn(tag: String, message: String) { Log.w(tag, message) }
        override fun logInfo(tag: String, message: String) { Log.i(tag, message) }
        override fun logDebug(tag: String, message: String) { Log.d(tag, message) }
        override fun logVerbose(tag: String, message: String) { Log.v(tag, message) }
        override fun logStackTraceWithMessage(tag: String, message: String, e: Exception) { Log.e(tag, message, e) }
        override fun logStackTrace(tag: String, e: Exception) { Log.e(tag, "", e) }
    }

    // ---- settings -----------------------------------------------------------

    private fun setupSettings() {
        val p = Env.prefs(this)
        val key = findViewById<EditText>(R.id.prefKey)
        val server = findViewById<EditText>(R.id.prefServer)
        key.setText(p.getString("key", ""))
        server.setText(p.getString("server", ""))
        findViewById<Button>(R.id.save).setOnClickListener {
            p.edit()
                .putString("key", key.text.toString().trim())
                .putString("server", server.text.toString().trim())
                .apply()
            MsnwService.start(this, restartProxy = true)
            bind()
            toast("saved")
        }
        findViewById<Button>(R.id.clearHttpAuth).setOnClickListener {
            httpAuthPrefs().edit().clear().commit()
            WebViewDatabase.getInstance(this).clearHttpAuthUsernamePassword()
            p.edit().putInt("web_profile_seq", p.getInt("web_profile_seq", 1) + 1).commit()
            tabs.filterIsInstance<Tab.Browser>().forEach { it.resetProfile() }
            window.decorView.post { cleanupOldWebProfiles() }
            toast("cleared saved HTTP passwords")
        }
    }

    private fun setupSshKey() {
        val pub = findViewById<TextView>(R.id.pubKey)
        if (Env.sshKey(this).exists()) pub.text = "(key exists; tap Generate / show key to display it)"
        findViewById<Button>(R.id.genKey).setOnClickListener {
            Thread {
                val text = try { Env.ensureSshKey(this) } catch (e: Exception) { "error: ${e.message}" }
                runOnUiThread { pub.text = text }
            }.start()
        }
        findViewById<Button>(R.id.copyKey).setOnClickListener {
            val t = pub.text.toString()
            if (t.startsWith("ssh-")) {
                (getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).setPrimaryClip(ClipData.newPlainText("msnw ssh key", t))
                toast("public key copied")
            }
        }
    }

    private fun setupVersion() {
        val v = findViewById<TextView>(R.id.version)
        val pkg = if (Build.VERSION.SDK_INT >= 33) {
            packageManager.getPackageInfo(packageName, PackageManager.PackageInfoFlags.of(0))
        } else {
            @Suppress("DEPRECATION") packageManager.getPackageInfo(packageName, 0)
        }
        val apkVer = pkg.versionName ?: "dev"
        v.text = "msnw $apkVer"
        Thread {
            val binVer = Env.msnwVersion(this)
            if (binVer != null && binVer != apkVer) {
                runOnUiThread { v.text = "msnw $apkVer (bin $binVer)" }
            }
        }.start()
    }

    private fun refreshLog() {
        findViewById<TextView>(R.id.log).text = MsnwService.logTail(this)
    }

    companion object {
        const val URL_HISTORY = "url_history"
        const val TARGET_HISTORY = "target_history"
        const val HISTORY_SHOWN = 5
        /** Finger travel that makes a flick, in dp. */
        const val FLICK_STEP_DP = 24
    }

    override fun onDestroy() {
        activeAuthDialog?.dismiss()
        activeAuthDialog = null
        tabs.filterIsInstance<Tab.Browser>().forEach { it.web.destroy() }
        // Sessions belong to the service and keep running; just stop receiving events.
        svc?.let { if (it.listener === sessionClient) it.listener = null }
        if (bound) unbindService(connection)
        svc = null
        super.onDestroy()
    }
}
