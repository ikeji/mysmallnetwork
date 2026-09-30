package ma.ikeji.msnw

import android.Manifest
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.util.Log
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.View
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputMethodManager
import android.webkit.WebChromeClient
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.webkit.ProxyConfig
import androidx.webkit.ProxyController
import androidx.webkit.WebViewFeature
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import com.termux.view.TerminalView
import com.termux.view.TerminalViewClient
import java.util.concurrent.Executors

class MainActivity : AppCompatActivity() {
    private lateinit var web: WebView
    private lateinit var url: EditText
    private lateinit var term: TerminalView
    private var session: TerminalSession? = null
    private var ctrlPending = false
    private var fontSize = 0

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        Env.setup(this)
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        setupBrowser()
        setupTerminal()
        setupSettings()
        findViewById<Button>(R.id.tabBrowser).setOnClickListener { show(R.id.browser) }
        findViewById<Button>(R.id.tabTerminal).setOnClickListener { show(R.id.terminal); ensureSession() }
        findViewById<Button>(R.id.tabSettings).setOnClickListener { show(R.id.settings); refreshLog() }
        val configured = !Env.prefs(this).getString("key", "").isNullOrBlank()
        if (configured) {
            MsnwService.start(this)
            show(R.id.browser)
            loadHome()
        } else {
            show(R.id.settings)
        }
    }

    private fun show(id: Int) {
        listOf(R.id.browser, R.id.terminal, R.id.settings).forEach {
            findViewById<View>(it).visibility = if (it == id) View.VISIBLE else View.GONE
        }
        if (id == R.id.terminal) term.requestFocus()
    }

    // ---- browser ------------------------------------------------------------

    private fun setupBrowser() {
        web = findViewById(R.id.web)
        url = findViewById(R.id.url)
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        web.settings.useWideViewPort = true
        web.settings.loadWithOverviewMode = true
        web.webViewClient = object : WebViewClient() {
            override fun onPageFinished(view: WebView?, u: String?) { url.setText(u ?: "") }
        }
        web.webChromeClient = WebChromeClient()
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
            Toast.makeText(this, "This WebView cannot use a proxy; update Android System WebView", Toast.LENGTH_LONG).show()
        }
        val go = { navigate(url.text.toString()) }
        findViewById<Button>(R.id.go).setOnClickListener { go() }
        url.setOnEditorActionListener { _, id, _ -> if (id == EditorInfo.IME_ACTION_GO) { go(); true } else false }
    }

    private fun navigate(raw: String) {
        var u = raw.trim()
        if (u.isEmpty()) return
        if (!u.contains("://")) u = "http://$u"
        web.loadUrl(u)
        (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).hideSoftInputFromWindow(url.windowToken, 0)
    }

    private fun loadHome() {
        val home = Env.prefs(this).getString("home", "") ?: ""
        if (home.isNotBlank()) navigate(home)
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        if (findViewById<View>(R.id.browser).visibility == View.VISIBLE && web.canGoBack()) web.goBack()
        else @Suppress("DEPRECATION") super.onBackPressed()
    }

    // ---- terminal -----------------------------------------------------------

    private fun setupTerminal() {
        term = findViewById(R.id.term)
        fontSize = (14 * resources.displayMetrics.density).toInt()
        term.setTextSize(fontSize)
        term.setTerminalViewClient(viewClient)
        term.keepScreenOn = true
        val send = { s: String -> session?.write(s) }
        findViewById<Button>(R.id.kEsc).setOnClickListener { send("\u001b") }
        findViewById<Button>(R.id.kTab).setOnClickListener { send("\t") }
        findViewById<Button>(R.id.kCtrl).setOnClickListener { ctrlPending = !ctrlPending; toast(if (ctrlPending) "Ctrl on" else "Ctrl off") }
        findViewById<Button>(R.id.kUp).setOnClickListener { send("\u001b[A") }
        findViewById<Button>(R.id.kDown).setOnClickListener { send("\u001b[B") }
        findViewById<Button>(R.id.kLeft).setOnClickListener { send("\u001b[D") }
        findViewById<Button>(R.id.kRight).setOnClickListener { send("\u001b[C") }
        findViewById<Button>(R.id.kReconnect).setOnClickListener { startSession() }
    }

    private fun ensureSession() {
        if (session?.isRunning != true) startSession()
    }

    private fun startSession() {
        session?.finishIfRunning()
        val p = Env.prefs(this)
        val target = p.getString("target", "") ?: ""
        if (target.isBlank() || p.getString("key", "").isNullOrBlank()) {
            toast("Set the link key and mosh target in Settings")
            show(R.id.settings)
            return
        }
        Env.setup(this)
        // TerminalSession passes args as the full argv, so args[0] is the program name.
        val args = arrayOf("msnw", "mosh", "-v", "-log", Env.logFile(this).absolutePath, target)
        val s = TerminalSession(Env.msnw(this).absolutePath, Env.home(this).absolutePath, args,
            Env.envArray(this), 2000, sessionClient)
        session = s
        term.attachSession(s)
        term.requestFocus()
        (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager).showSoftInput(term, 0)
    }

    private fun toast(s: String) = Toast.makeText(this, s, Toast.LENGTH_SHORT).show()

    private val sessionClient = object : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) { term.onScreenUpdated() }
        override fun onTitleChanged(changedSession: TerminalSession) {}
        override fun onSessionFinished(finishedSession: TerminalSession) {
            runOnUiThread { toast("session ended (exit ${finishedSession.exitStatus}); tap Reconnect") }
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
            show(R.id.browser)
            loadHome()
        }
    }

    private fun refreshLog() {
        findViewById<TextView>(R.id.log).text = MsnwService.logTail(this)
    }

    override fun onDestroy() {
        session?.finishIfRunning()
        super.onDestroy()
    }
}
