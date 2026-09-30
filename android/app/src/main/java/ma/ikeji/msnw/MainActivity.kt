package ma.ikeji.msnw

import android.Manifest
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.pm.PackageManager
import android.graphics.Color
import android.os.Build
import android.os.Bundle
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
import androidx.webkit.ProxyConfig
import androidx.webkit.ProxyController
import androidx.webkit.WebViewFeature
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import com.termux.view.TerminalView
import com.termux.view.TerminalViewClient
import java.util.concurrent.Executors

/**
 * One activity with a strip of tabs. A browser tab owns a WebView (routed
 * through the local HTTP proxy); a terminal tab owns a TerminalSession
 * running "msnw mosh" and is shown in the single TerminalView. Sessions keep
 * running while another tab is in front. Long-press a tab to close it.
 */
class MainActivity : AppCompatActivity() {
    private sealed class Tab(val button: Button) {
        class Browser(button: Button, val view: View, val web: WebView, val url: EditText) : Tab(button)
        class Term(button: Button, var session: TerminalSession?) : Tab(button)
    }

    private val tabs = mutableListOf<Tab>()
    private var current: Tab? = null
    private var seq = 0

    private lateinit var strip: LinearLayout
    private lateinit var content: FrameLayout
    private lateinit var term: TerminalView
    private var ctrlPending = false
    private var fontSize = 0

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
        findViewById<Button>(R.id.newBrowser).setOnClickListener { select(newBrowserTab(homeUrl())) }
        findViewById<Button>(R.id.newTerminal).setOnClickListener { newTerminalTab() }
        findViewById<Button>(R.id.tabSettings).setOnClickListener { showSettings() }
        if (Env.prefs(this).getString("key", "").isNullOrBlank()) {
            showSettings()
        } else {
            MsnwService.start(this)
            select(newBrowserTab(homeUrl()))
        }
    }

    private fun homeUrl() = Env.prefs(this).getString("home", "") ?: ""

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
            tab.session?.let { term.attachSession(it) }
            term.requestFocus()
        }
    }

    private fun closeTab(tab: Tab) {
        when (tab) {
            is Tab.Browser -> { content.removeView(tab.view); tab.web.destroy() }
            is Tab.Term -> tab.session?.finishIfRunning()
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
        val button = makeTabButton("B${++seq}") { tab }
        tab = Tab.Browser(button, column, web, url)
        tabs.add(tab)
        web.webViewClient = object : WebViewClient() {
            override fun onPageFinished(view: WebView?, u: String?) { url.setText(u ?: "") }
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
        val send = { s: String -> (current as? Tab.Term)?.session?.write(s) }
        findViewById<Button>(R.id.kEsc).setOnClickListener { send("\u001b") }
        findViewById<Button>(R.id.kTab).setOnClickListener { send("\t") }
        findViewById<Button>(R.id.kCtrl).setOnClickListener { ctrlPending = !ctrlPending; toast(if (ctrlPending) "Ctrl on" else "Ctrl off") }
        findViewById<Button>(R.id.kUp).setOnClickListener { send("\u001b[A") }
        findViewById<Button>(R.id.kDown).setOnClickListener { send("\u001b[B") }
        findViewById<Button>(R.id.kLeft).setOnClickListener { send("\u001b[D") }
        findViewById<Button>(R.id.kRight).setOnClickListener { send("\u001b[C") }
        findViewById<Button>(R.id.kReconnect).setOnClickListener { (current as? Tab.Term)?.let { startSession(it) } }
        findViewById<Button>(R.id.kClose).setOnClickListener { (current as? Tab.Term)?.let { closeTab(it) } }
    }

    private fun newTerminalTab() {
        val p = Env.prefs(this)
        if (p.getString("target", "").isNullOrBlank() || p.getString("key", "").isNullOrBlank()) {
            toast("Set the link key and mosh target in Settings")
            showSettings()
            return
        }
        lateinit var tab: Tab.Term
        val button = makeTabButton("T${++seq}") { tab }
        tab = Tab.Term(button, null)
        tabs.add(tab)
        startSession(tab)
        select(tab)
    }

    private fun startSession(tab: Tab.Term) {
        tab.session?.finishIfRunning()
        val target = Env.prefs(this).getString("target", "") ?: ""
        Env.setup(this)
        tab.button.text = tab.button.text.toString().trimEnd('!')
        // Building the environment resolves the server name (network I/O), so do it off the UI thread.
        Thread {
            val env = Env.envArray(this)
            runOnUiThread {
                if (tab !in tabs) return@runOnUiThread
                // TerminalSession passes args as the full argv, so args[0] is the program name.
                val args = arrayOf("msnw", "mosh", "-v", "-log", Env.logFile(this).absolutePath, target)
                val s = TerminalSession(Env.msnw(this).absolutePath, Env.home(this).absolutePath, args,
                    env, 2000, sessionClient)
                tab.session = s
                if (current === tab) term.attachSession(s)
                term.requestFocus()
                (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).showSoftInput(term, 0)
            }
        }.start()
    }

    private fun termTabOf(session: TerminalSession): Tab.Term? =
        tabs.filterIsInstance<Tab.Term>().firstOrNull { it.session === session }

    private fun toast(s: String) = Toast.makeText(this, s, Toast.LENGTH_SHORT).show()

    private val sessionClient = object : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) {
            if ((current as? Tab.Term)?.session === changedSession) term.onScreenUpdated()
        }
        override fun onTitleChanged(changedSession: TerminalSession) {}
        override fun onSessionFinished(finishedSession: TerminalSession) {
            runOnUiThread {
                termTabOf(finishedSession)?.button?.let { it.text = it.text.toString().trimEnd('!') + "!" }
                if ((current as? Tab.Term)?.session === finishedSession) toast("session ended (exit ${finishedSession.exitStatus}); Reconnect or close the tab")
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
        override fun onSingleTapUp(e: MotionEvent) {
            (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).showSoftInput(term, 0)
        }
        override fun shouldBackButtonBeMappedToEscape() = false
        override fun shouldEnforceCharBasedInput() = true
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
        val target = findViewById<EditText>(R.id.prefTarget)
        val home = findViewById<EditText>(R.id.prefHome)
        val server = findViewById<EditText>(R.id.prefServer)
        key.setText(p.getString("key", ""))
        target.setText(p.getString("target", ""))
        home.setText(p.getString("home", ""))
        server.setText(p.getString("server", ""))
        findViewById<Button>(R.id.save).setOnClickListener {
            p.edit()
                .putString("key", key.text.toString().trim())
                .putString("target", target.text.toString().trim())
                .putString("home", home.text.toString().trim())
                .putString("server", server.text.toString().trim())
                .apply()
            MsnwService.start(this) // restarts the proxy with the new settings
            toast("saved")
            val first = tabs.firstOrNull()
            if (first != null) select(first) else select(newBrowserTab(homeUrl()))
        }
    }

    private fun refreshLog() {
        findViewById<TextView>(R.id.log).text = MsnwService.logTail(this)
    }

    override fun onDestroy() {
        tabs.filterIsInstance<Tab.Term>().forEach { it.session?.finishIfRunning() }
        super.onDestroy()
    }
}
