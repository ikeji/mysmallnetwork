package ma.ikeji.msnw

import android.Manifest
import android.content.ClipData
import android.content.ClipboardManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.graphics.Color
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import android.text.InputType
import android.util.Log
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.View
import android.view.ViewGroup
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputMethodManager
import android.webkit.WebChromeClient
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.webkit.ProxyConfig
import androidx.webkit.ProxyController
import androidx.webkit.WebViewFeature
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import com.termux.view.TerminalView
import com.termux.view.TerminalViewClient
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
        class Browser(button: Button, val view: View, val web: WebView, val url: EditText) : Tab(button)
        class Term(button: Button, val entry: MsnwService.Entry) : Tab(button)
    }

    private val tabs = mutableListOf<Tab>()
    private var current: Tab? = null

    private lateinit var strip: LinearLayout
    private lateinit var content: FrameLayout
    private lateinit var term: TerminalView
    private var ctrlPending = false
    private var fontSize = 0

    private var svc: MsnwService? = null
    private var bound = false
    private val pending = mutableListOf<(MsnwService) -> Unit>()
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
        setupProxy()
        setupTerminal()
        setupSettings()
        setupSshKey()
        setupLauncher()
        findViewById<Button>(R.id.tabSettings).setOnClickListener { showSettings() }
        if (!Env.prefs(this).getString("key", "").isNullOrBlank()) {
            MsnwService.start(this)
            bind()
        }
        showSettings() // the "+" screen: open tabs from history or by typing
    }

    // ---- "+" screen: open web / mosh tabs ------------------------------------

    private fun setupLauncher() {
        val urlEdit = findViewById<EditText>(R.id.openUrl)
        val openUrl = {
            val u = urlEdit.text.toString().trim()
            if (u.isNotEmpty()) { urlEdit.setText(""); select(newBrowserTab(if (u.contains("://")) u else "http://$u")) }
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

    /** Fills a history list with one row per entry: tap opens, long-press forgets. */
    private fun fillHistory(container: LinearLayout, key: String, open: (String) -> Unit) {
        container.removeAllViews()
        Env.history(this, key).forEach { item ->
            val row = Button(this, null, android.R.attr.borderlessButtonStyle)
            row.text = item
            row.isAllCaps = false
            row.setTextColor(Color.WHITE)
            row.gravity = android.view.Gravity.START or android.view.Gravity.CENTER_VERTICAL
            row.setOnClickListener { open(item) }
            row.setOnLongClickListener { Env.removeHistory(this, key, item); fillHistory(container, key, open); true }
            container.addView(row, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
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
        if (tab is Tab.Term) {
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
        if (WebViewFeature.isFeatureSupported(WebViewFeature.PROXY_OVERRIDE)) {
            val config = ProxyConfig.Builder()
                .addProxyRule("http://127.0.0.1:${MsnwService.PROXY_PORT}")
                .addBypassRule("127.0.0.1")
                .addBypassRule("localhost")
                .build()
            ProxyController.getInstance().setProxyOverride(config, Executors.newSingleThreadExecutor()) {
                Log.i(MsnwService.TAG, "webview proxy set")
            }
        } else {
            toast("This WebView cannot use a proxy; update Android System WebView")
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
        bar.addView(url, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
        bar.addView(go)
        val web = WebView(this)
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        web.settings.useWideViewPort = true
        web.settings.loadWithOverviewMode = true
        column.addView(bar, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        column.addView(web, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
        content.addView(column, 0, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))

        lateinit var tab: Tab.Browser
        val button = makeTabButton("B") { tab }
        tab = Tab.Browser(button, column, web, url)
        tabs.add(tab)
        web.webViewClient = object : WebViewClient() {
            override fun onPageFinished(view: WebView?, u: String?) {
                url.setText(u ?: "")
                if (!u.isNullOrBlank() && u != "about:blank") Env.addHistory(this@MainActivity, URL_HISTORY, u)
            }
        }
        web.webChromeClient = object : WebChromeClient() {
            override fun onReceivedTitle(view: WebView?, title: String?) {
                if (!title.isNullOrBlank()) button.text = title.take(12)
            }
        }
        val navigate = {
            var u = url.text.toString().trim()
            if (u.isNotEmpty()) {
                if (!u.contains("://")) u = "http://$u"
                web.loadUrl(u)
                (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).hideSoftInputFromWindow(url.windowToken, 0)
            }
        }
        go.setOnClickListener { navigate() }
        url.setOnEditorActionListener { _, id, _ -> if (id == EditorInfo.IME_ACTION_GO) { navigate(); true } else false }
        if (initialUrl.isNotBlank()) { url.setText(initialUrl); navigate() }
        return tab
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        val c = current
        if (c is Tab.Browser && c.web.canGoBack()) c.web.goBack()
        else @Suppress("DEPRECATION") super.onBackPressed()
    }

    // ---- terminal tabs ------------------------------------------------------

    private fun setupTerminal() {
        term = findViewById(R.id.term)
        fontSize = (14 * resources.displayMetrics.density).toInt()
        term.setTextSize(fontSize)
        term.setTerminalViewClient(viewClient)
        term.keepScreenOn = true
        val send = { s: String -> (current as? Tab.Term)?.entry?.session?.write(s) }
        findViewById<Button>(R.id.kEsc).setOnClickListener { send("\u001b") }
        findViewById<Button>(R.id.kTab).setOnClickListener { send("\t") }
        findViewById<Button>(R.id.kCtrl).setOnClickListener { ctrlPending = !ctrlPending; toast(if (ctrlPending) "Ctrl on" else "Ctrl off") }
        findViewById<Button>(R.id.kUp).setOnClickListener { send("\u001b[A") }
        findViewById<Button>(R.id.kDown).setOnClickListener { send("\u001b[B") }
        findViewById<Button>(R.id.kLeft).setOnClickListener { send("\u001b[D") }
        findViewById<Button>(R.id.kRight).setOnClickListener { send("\u001b[C") }
        findViewById<Button>(R.id.kReconnect).setOnClickListener { (current as? Tab.Term)?.let { restartSession(it) } }
        findViewById<Button>(R.id.kClose).setOnClickListener { (current as? Tab.Term)?.let { closeTab(it) } }
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

    /** Tab label: the title set by the shell (OSC 0/2), else T<id>; "!" marks a finished session. */
    private fun termLabel(tab: Tab.Term): String {
        val title = tab.entry.session.title?.takeIf { it.isNotBlank() }?.take(16) ?: "T${tab.entry.id}"
        return if (tab.entry.finished) "$title!" else title
    }

    private fun addTermTab(entry: MsnwService.Entry): Tab.Term {
        lateinit var tab: Tab.Term
        val button = makeTabButton("") { tab }
        tab = Tab.Term(button, entry)
        button.text = termLabel(tab)
        tabs.add(tab)
        return tab
    }

    private fun restartSession(tab: Tab.Term) {
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
        override fun readAltKey() = false
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

    private fun refreshLog() {
        findViewById<TextView>(R.id.log).text = MsnwService.logTail(this)
    }

    companion object {
        const val URL_HISTORY = "url_history"
        const val TARGET_HISTORY = "target_history"
    }

    override fun onDestroy() {
        // Sessions belong to the service and keep running; just stop receiving events.
        svc?.let { if (it.listener === sessionClient) it.listener = null }
        if (bound) unbindService(connection)
        svc = null
        super.onDestroy()
    }
}
