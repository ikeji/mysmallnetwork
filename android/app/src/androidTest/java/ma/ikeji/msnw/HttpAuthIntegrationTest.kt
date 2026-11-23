package ma.ikeji.msnw

import android.content.Context
import android.content.DialogInterface
import android.content.Intent
import android.os.SystemClock
import android.util.Base64
import android.view.View
import android.view.ViewGroup
import android.webkit.WebView
import android.webkit.WebViewDatabase
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AlertDialog
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.io.BufferedReader
import java.io.ByteArrayOutputStream
import java.io.InputStreamReader
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.nio.charset.Charset
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

/**
 * End-to-end integration test for HTTP Basic authentication in the built-in
 * browser: exercises MainActivity, real WebView + ProxyController, a local
 * msnw server + msnw export + msnw http-proxy tunnel ("http://authbox/"),
 * UTF-8 and Shift_JIS realm decoding, credential persistence, and clearing
 * saved passwords (verifying both re-prompting and subsequent login).
 */
class HttpAuthIntegrationTest {
    private lateinit var httpServer: BasicAuthTestServer
    private var relayProc: Process? = null
    private var exportProc: Process? = null
    private var activity: MainActivity? = null

    private val linkKey = "integration-test-secret-link-key-0123456789"

    @Before
    fun setUp() {
        val inst = InstrumentationRegistry.getInstrumentation()
        val ctx = inst.targetContext
        Env.setup(ctx)

        httpServer = BasicAuthTestServer().also { it.start() }
        val ctrlPort = freeUdpPort()
        val relayPort = freeUdpPort()

        // Start a local msnw rendezvous/relay server and export the HTTP server as "authbox".
        val msnwBin = Env.msnw(ctx).absolutePath
        relayProc = ProcessBuilder(
            msnwBin, "server",
            "-listen", "127.0.0.1:$ctrlPort",
            "-relay", "127.0.0.1:$relayPort"
        ).directory(Env.home(ctx)).redirectErrorStream(true).start()

        val expReady = CountDownLatch(1)
        exportProc = ProcessBuilder(
            msnwBin, "export",
            "-s", "127.0.0.1:$ctrlPort",
            "-key", linkKey,
            "-n", "authbox",
            "-t", "127.0.0.1:${httpServer.port}"
        ).directory(Env.home(ctx)).redirectErrorStream(true).start().also { p ->
            Thread {
                try {
                    p.inputStream.bufferedReader().forEachLine { line ->
                        if (line.contains("registered") || line.contains("exporting") || line.contains("authbox")) {
                            expReady.countDown()
                        }
                    }
                } catch (_: Exception) {}
            }.apply { isDaemon = true; start() }
        }
        // Give msnw server + exporter a moment to register on loopback.
        expReady.await(3, TimeUnit.SECONDS)
        SystemClock.sleep(500)

        // Reset app state and point MsnwService at the local test relay.
        ctx.getSharedPreferences("msnw_http_auth", Context.MODE_PRIVATE).edit().clear().commit()
        inst.runOnMainSync {
            WebViewDatabase.getInstance(ctx).clearHttpAuthUsernamePassword()
        }
        Env.prefs(ctx).edit()
            .clear()
            .putString("key", linkKey)
            .putString("server", "127.0.0.1:$ctrlPort")
            .commit()
        MsnwService.start(ctx, restartProxy = true)
        waitForTcp("127.0.0.1", MsnwService.PROXY_PORT, 10_000)

        val intent = Intent(ctx, MainActivity::class.java).apply {
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TASK)
        }
        activity = inst.startActivitySync(intent) as MainActivity
    }

    @After
    fun tearDown() {
        val inst = InstrumentationRegistry.getInstrumentation()
        val ctx = inst.targetContext
        activity?.let { act ->
            inst.runOnMainSync { act.finish() }
        }
        ctx.stopService(Intent(ctx, MsnwService::class.java))
        exportProc?.destroyForcibly()
        relayProc?.destroyForcibly()
        httpServer.close()
    }

    @Test
    fun basicAuth_japaneseRealm_save_clear_and_relogin_overMsnwTunnel() {
        val act = activity!!

        // 1. Open http://authbox/ through msnw http-proxy -> msnw tunnel -> BasicAuthTestServer.
        openUrlFromPlusScreen(act, "http://authbox/")

        val d1 = waitForAuthDialog(act)
        val msg1 = dialogMessage(act, d1)
        assertTrue("Expected decoded UTF-8 Japanese realm in '$msg1'", msg1.contains("管理画面"))

        // 2. Submit wrong password -> must re-prompt (no infinite loop) with username pre-filled.
        submitAuthDialog(act, d1, "alice", "wrong-password")
        val d2 = waitForNewAuthDialog(act, previous = d1)
        assertEquals("alice", dialogFields(act, d2).first.text.toString())

        // 3. Submit valid alice credentials -> main page AND authenticated /sub.js must load.
        submitAuthDialog(act, d2, "alice", "secret123")
        waitForPageText(act, "AUTH_OK:alice:SUB_OK")

        // 4. Open http://authbox/ in a second browser tab -> saved credentials auto-login without dialog.
        onUi(act) { act.findViewById<Button>(R.id.tabSettings).performClick() }
        openUrlFromPlusScreen(act, "http://authbox/")
        waitForPageText(act, "AUTH_OK:alice:SUB_OK")
        assertNull("Saved credentials should not show dialog", onUiResult(act) { act.activeAuthDialog })

        // 5. Tap "Clear saved HTTP passwords", switch back to existing tab 0 ->
        //    MUST re-prompt AND entering bob's password MUST succeed!
        onUi(act) {
            act.findViewById<Button>(R.id.tabSettings).performClick()
            act.findViewById<Button>(R.id.clearHttpAuth).performClick()
        }
        onUi(act) {
            val strip = act.findViewById<LinearLayout>(R.id.tabs)
            (strip.getChildAt(0) as Button).performClick()
        }
        val d3 = waitForAuthDialog(act, "Expected auth dialog after clearing saved passwords on open tab")
        assertEquals("", dialogFields(act, d3).first.text.toString())
        submitAuthDialog(act, d3, "bob", "pass456")
        waitForPageText(act, "AUTH_OK:bob:SUB_OK")

        // 6. Clear again and open from the URL history list on the "+" screen ->
        //    MUST re-prompt AND entering alice's password MUST succeed!
        onUi(act) {
            act.findViewById<Button>(R.id.tabSettings).performClick()
            act.findViewById<Button>(R.id.clearHttpAuth).performClick()
        }
        onUi(act) {
            val hist = act.findViewById<LinearLayout>(R.id.urlHistory)
            val row = hist.getChildAt(0) as LinearLayout
            (row.getChildAt(0) as Button).performClick()
        }
        val d4 = waitForAuthDialog(act, "Expected auth dialog when opening from history after clear")
        submitAuthDialog(act, d4, "alice", "secret123")
        waitForPageText(act, "AUTH_OK:alice:SUB_OK")

        // 7. Clear again (otherwise RFC 7617 preemptively reuses "/" credentials for "/sjis")
        //    and verify Shift_JIS realm decoding as well.
        onUi(act) {
            act.findViewById<Button>(R.id.tabSettings).performClick()
            act.findViewById<Button>(R.id.clearHttpAuth).performClick()
        }
        openUrlFromPlusScreen(act, "http://authbox/sjis")
        val d5 = waitForAuthDialog(act, "Expected auth dialog for Shift_JIS realm")
        val msg5 = dialogMessage(act, d5)
        assertTrue("Expected decoded Shift_JIS realm in '$msg5'", msg5.contains("日本語認証"))
        submitAuthDialog(act, d5, "alice", "secret123")
        waitForPageText(act, "SJIS_OK:alice")
    }

    // ---- UI & polling helpers -----------------------------------------------

    private fun openUrlFromPlusScreen(act: MainActivity, url: String) {
        onUi(act) {
            act.findViewById<EditText>(R.id.openUrl).setText(url)
            act.findViewById<Button>(R.id.openUrlBtn).performClick()
        }
    }

    private fun waitForAuthDialog(act: MainActivity, failMsg: String = "Timed out waiting for Sign in dialog"): AlertDialog {
        val deadline = SystemClock.uptimeMillis() + 15_000
        while (SystemClock.uptimeMillis() < deadline) {
            val d = onUiResult(act) { act.activeAuthDialog?.takeIf { it.isShowing } }
            if (d != null) return d
            SystemClock.sleep(50)
        }
        throw AssertionError(failMsg)
    }

    private fun waitForNewAuthDialog(act: MainActivity, previous: AlertDialog): AlertDialog {
        val deadline = SystemClock.uptimeMillis() + 15_000
        while (SystemClock.uptimeMillis() < deadline) {
            val d = onUiResult(act) { act.activeAuthDialog?.takeIf { it.isShowing && it !== previous } }
            if (d != null) return d
            SystemClock.sleep(50)
        }
        throw AssertionError("Timed out waiting for re-prompt dialog after bad credentials")
    }

    private fun dialogMessage(act: MainActivity, dialog: AlertDialog): String =
        onUiResult(act) { dialog.findViewById<TextView>(android.R.id.message)?.text?.toString() ?: "" }

    private fun dialogFields(act: MainActivity, dialog: AlertDialog): Pair<EditText, EditText> =
        onUiResult(act) {
            val edits = mutableListOf<EditText>()
            fun collect(v: View?) {
                if (v is EditText) edits += v
                else if (v is ViewGroup) for (i in 0 until v.childCount) collect(v.getChildAt(i))
            }
            collect(dialog.window?.decorView)
            assertEquals("Expected username and password fields", 2, edits.size)
            edits[0] to edits[1]
        }

    private fun submitAuthDialog(act: MainActivity, dialog: AlertDialog, user: String, pass: String) {
        onUi(act) {
            val (uEdit, pEdit) = dialogFieldsInUi(dialog)
            uEdit.setText(user)
            pEdit.setText(pass)
            dialog.getButton(DialogInterface.BUTTON_POSITIVE).performClick()
        }
    }

    private fun dialogFieldsInUi(dialog: AlertDialog): Pair<EditText, EditText> {
        val edits = mutableListOf<EditText>()
        fun collect(v: View?) {
            if (v is EditText) edits += v
            else if (v is ViewGroup) for (i in 0 until v.childCount) collect(v.getChildAt(i))
        }
        collect(dialog.window?.decorView)
        assertEquals(2, edits.size)
        return edits[0] to edits[1]
    }

    private fun waitForPageText(act: MainActivity, expected: String) {
        val deadline = SystemClock.uptimeMillis() + 15_000
        var last = ""
        while (SystemClock.uptimeMillis() < deadline) {
            val web = onUiResult(act) { act.currentBrowserWebView() }
            if (web != null) {
                val body = evalJs(act, web, "document.body ? document.body.innerText : ''")
                last = body
                if (body.contains(expected)) return
            }
            SystemClock.sleep(100)
        }
        val activeDialog = onUiResult(act) { act.activeAuthDialog?.isShowing == true }
        throw AssertionError("Timed out waiting for '$expected' (last body='$last', authDialogShowing=$activeDialog)")
    }

    private fun evalJs(act: MainActivity, web: WebView, script: String): String {
        val latch = CountDownLatch(1)
        val ref = AtomicReference("")
        onUi(act) {
            web.evaluateJavascript(script) { raw ->
                // evaluateJavascript JSON-encodes string results ("foo").
                val unquoted = if (raw != null && raw.length >= 2 && raw.startsWith("\"") && raw.endsWith("\"")) {
                    raw.substring(1, raw.length - 1)
                } else {
                    raw ?: ""
                }
                ref.set(unquoted)
                latch.countDown()
            }
        }
        latch.await(5, TimeUnit.SECONDS)
        return ref.get()
    }

    private fun onUi(act: MainActivity, block: () -> Unit) {
        InstrumentationRegistry.getInstrumentation().runOnMainSync(block)
        assertNotNull(act)
    }

    private fun <T> onUiResult(act: MainActivity, block: () -> T): T {
        val ref = AtomicReference<T>()
        onUi(act) { ref.set(block()) }
        return ref.get()
    }

    private fun freeUdpPort(): Int = DatagramSocket(0, InetAddress.getByName("127.0.0.1")).use { it.localPort }

    private fun waitForTcp(host: String, port: Int, timeoutMs: Long) {
        val deadline = SystemClock.uptimeMillis() + timeoutMs
        while (SystemClock.uptimeMillis() < deadline) {
            try {
                Socket(host, port).use { return }
            } catch (_: Exception) {
                SystemClock.sleep(100)
            }
        }
        throw AssertionError("Proxy $host:$port did not start in time")
    }

    /**
     * Minimal raw-socket HTTP server supporting Basic Auth with two valid users
     * ("alice:secret123" and "bob:pass456") and raw UTF-8 / Shift_JIS realm bytes.
     */
    private class BasicAuthTestServer : AutoCloseable {
        private val server = ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"))
        val port: Int get() = server.localPort
        private val users = mapOf("alice" to "secret123", "bob" to "pass456")
        @Volatile private var closed = false

        fun start() {
            Thread {
                while (!closed) {
                    val sock = try { server.accept() } catch (_: Exception) { break }
                    Thread { handle(sock) }.apply { isDaemon = true; start() }
                }
            }.apply { isDaemon = true; start() }
        }

        private fun handle(sock: Socket) {
            sock.use { s ->
                s.soTimeout = 5_000
                val reader = BufferedReader(InputStreamReader(s.getInputStream(), Charsets.ISO_8859_1))
                val reqLine = reader.readLine() ?: return
                val parts = reqLine.split(' ')
                if (parts.size < 2) return
                val path = parts[1]
                var authHeader: String? = null
                while (true) {
                    val line = reader.readLine() ?: break
                    if (line.isEmpty()) break
                    if (line.startsWith("Authorization:", ignoreCase = true)) {
                        authHeader = line.substringAfter(':').trim()
                    }
                }
                val authedUser = parseBasicUser(authHeader)
                val isSjis = path.startsWith("/sjis")
                if (authedUser == null) {
                    val realmBytes = if (isSjis) {
                        "日本語認証".toByteArray(Charset.forName("Windows-31J"))
                    } else {
                        "管理画面".toByteArray(Charsets.UTF_8)
                    }
                    val body = "Unauthorized".toByteArray(Charsets.UTF_8)
                    val out = ByteArrayOutputStream()
                    out.write("HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Basic realm=\"".toByteArray(Charsets.ISO_8859_1))
                    out.write(realmBytes)
                    out.write("\"\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: ${body.size}\r\nConnection: close\r\n\r\n".toByteArray(Charsets.ISO_8859_1))
                    out.write(body)
                    s.getOutputStream().write(out.toByteArray())
                    s.getOutputStream().flush()
                    return
                }
                val (ctype, bodyStr) = when {
                    path.startsWith("/sub.js") ->
                        "application/javascript; charset=utf-8" to "document.getElementById('out').textContent += ':SUB_OK';"
                    isSjis ->
                        "text/html; charset=utf-8" to "<!doctype html><meta charset='utf-8'><body>SJIS_OK:$authedUser</body>"
                    else ->
                        "text/html; charset=utf-8" to "<!doctype html><meta charset='utf-8'><title>Secret</title><body><div id='out'>AUTH_OK:$authedUser</div><script src='/sub.js'></script></body>"
                }
                val body = bodyStr.toByteArray(Charsets.UTF_8)
                val head = "HTTP/1.1 200 OK\r\nContent-Type: $ctype\r\nCache-Control: no-store\r\nContent-Length: ${body.size}\r\nConnection: close\r\n\r\n"
                s.getOutputStream().write(head.toByteArray(Charsets.ISO_8859_1))
                s.getOutputStream().write(body)
                s.getOutputStream().flush()
            }
        }

        private fun parseBasicUser(header: String?): String? {
            if (header == null || !header.startsWith("Basic ", ignoreCase = true)) return null
            val b64 = header.substring(6).trim()
            val decoded = try { String(Base64.decode(b64, Base64.DEFAULT), Charsets.UTF_8) } catch (_: Exception) { return null }
            val idx = decoded.indexOf(':')
            if (idx < 0) return null
            val u = decoded.substring(0, idx)
            val p = decoded.substring(idx + 1)
            return if (users[u] == p) u else null
        }

        override fun close() {
            closed = true
            try { server.close() } catch (_: Exception) {}
        }
    }
}
