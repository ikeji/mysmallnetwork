package ma.ikeji.msnw

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Binder
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import java.io.File

/**
 * Foreground service that owns everything long-lived: the msnw HTTP proxy
 * process for the browser, and the terminal sessions running "msnw mosh".
 * The activity binds to it and only attaches views, so sessions survive the
 * activity being destroyed (back key, swipe from recents, memory pressure).
 * A partial wake lock keeps the tunnels alive while the screen is off.
 */
class MsnwService : Service() {
    /** One terminal session and what the activity needs to show it. */
    class Entry(val id: Int, val target: String, var session: TerminalSession, var finished: Boolean = false)

    inner class LocalBinder : Binder() {
        val service: MsnwService get() = this@MsnwService
    }

    private val binder = LocalBinder()
    private var proc: Process? = null
    private var thread: Thread? = null
    @Volatile private var stopping = false
    private var wakeLock: PowerManager.WakeLock? = null

    val entries = mutableListOf<Entry>()
    private var nextId = 1
    /** The activity's client, forwarded session events while bound. */
    @Volatile var listener: TerminalSessionClient? = null

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }
        startForeground(1, notification())
        if (wakeLock == null) {
            wakeLock = (getSystemService(Context.POWER_SERVICE) as PowerManager)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "msnw:proxy").also { it.acquire() }
        }
        if (intent?.action == ACTION_RESTART_PROXY || proc == null) restartProxy()
        return START_STICKY
    }

    // ---- terminal sessions --------------------------------------------------

    /** Starts "msnw mosh" for target in a new pty. Blocking (DNS); call off the main thread. */
    fun newSession(target: String): Entry {
        Env.setup(this)
        val env = Env.envArray(this)
        val id = nextId++
        val entry = Entry(id, target, spawn(target, env))
        synchronized(entries) { entries += entry }
        updateNotification()
        return entry
    }

    /** Replaces a finished (or stuck) session with a fresh one for the same target. */
    fun restart(entry: Entry) {
        entry.session.finishIfRunning()
        entry.session = spawn(entry.target, Env.envArray(this))
        entry.finished = false
    }

    fun close(entry: Entry) {
        entry.session.finishIfRunning()
        synchronized(entries) { entries -= entry }
        updateNotification()
    }

    private fun spawn(target: String, env: Array<String>): TerminalSession {
        // TerminalSession passes args as the full argv, so args[0] is the program name.
        val args = mutableListOf("msnw", "mosh", "-v", "-log", Env.logFile(this).absolutePath)
        if (Env.sshKey(this).exists()) args += listOf("-ssh", "-i ${Env.sshKey(this).absolutePath}")
        args += target
        return TerminalSession(Env.msnw(this).absolutePath, Env.home(this).absolutePath, args.toTypedArray(),
            env, 2000, sessionClient)
    }

    fun entryOf(session: TerminalSession): Entry? = synchronized(entries) { entries.firstOrNull { it.session === session } }

    /** Receives events for every session and forwards them to the bound activity. */
    private val sessionClient = object : TerminalSessionClient {
        override fun onTextChanged(s: TerminalSession) { listener?.onTextChanged(s) }
        override fun onTitleChanged(s: TerminalSession) { listener?.onTitleChanged(s) }
        override fun onSessionFinished(s: TerminalSession) {
            entryOf(s)?.finished = true
            updateNotification()
            listener?.onSessionFinished(s)
        }
        override fun onCopyTextToClipboard(s: TerminalSession, text: String) { listener?.onCopyTextToClipboard(s, text) }
        override fun onPasteTextFromClipboard(s: TerminalSession?) { listener?.onPasteTextFromClipboard(s) }
        override fun onBell(s: TerminalSession) { listener?.onBell(s) }
        override fun onColorsChanged(s: TerminalSession) { listener?.onColorsChanged(s) }
        override fun onTerminalCursorStateChange(state: Boolean) { listener?.onTerminalCursorStateChange(state) }
        override fun setTerminalShellPid(s: TerminalSession, pid: Int) {}
        override fun getTerminalCursorStyle(): Int? = null
        override fun logError(tag: String, message: String) { Log.e(tag, message) }
        override fun logWarn(tag: String, message: String) { Log.w(tag, message) }
        override fun logInfo(tag: String, message: String) { Log.i(tag, message) }
        override fun logDebug(tag: String, message: String) { Log.d(tag, message) }
        override fun logVerbose(tag: String, message: String) { Log.v(tag, message) }
        override fun logStackTraceWithMessage(tag: String, message: String, e: Exception) { Log.e(tag, message, e) }
        override fun logStackTrace(tag: String, e: Exception) { Log.e(tag, "", e) }
    }

    // ---- proxy process ------------------------------------------------------

    /** (Re)starts the msnw proxy process with the current settings. */
    private fun restartProxy() {
        stopping = true
        proc?.destroy()
        thread?.join(2000)
        stopping = false
        thread = Thread { loop() }.also { it.isDaemon = true; it.start() }
    }

    private fun loop() {
        Env.setup(this)
        var backoff = 1000L
        while (!stopping) {
            val key = Env.prefs(this).getString("key", "") ?: ""
            if (key.isBlank()) {
                Log.w(TAG, "no link key configured; not starting msnw")
                return
            }
            val cmd = listOf(Env.msnw(this).absolutePath, "client", "--http-proxy", "127.0.0.1:$PROXY_PORT",
                "-log", Env.logFile(this).absolutePath)
            val pb = ProcessBuilder(cmd).directory(Env.home(this)).redirectErrorStream(true)
            pb.environment().clear(); pb.environment().putAll(Env.environment(this))
            val started = System.currentTimeMillis()
            try {
                val p = pb.start().also { proc = it }
                p.inputStream.bufferedReader().use { r -> r.lineSequence().forEach { Log.i(TAG, it) } }
                val code = p.waitFor()
                Log.w(TAG, "msnw exited with $code")
            } catch (e: Exception) {
                Log.e(TAG, "msnw failed", e)
            }
            if (stopping) return
            backoff = if (System.currentTimeMillis() - started > 60_000) 1000L else minOf(backoff * 2, 30_000L)
            try { Thread.sleep(backoff) } catch (_: InterruptedException) { return }
        }
    }

    override fun onDestroy() {
        stopping = true
        proc?.destroy()
        synchronized(entries) { entries.forEach { it.session.finishIfRunning() }; entries.clear() }
        wakeLock?.let { if (it.isHeld) it.release() }
        super.onDestroy()
    }

    // ---- notification -------------------------------------------------------

    private fun updateNotification() {
        (getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager).notify(1, notification())
    }

    private fun notification(): Notification {
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.createNotificationChannel(NotificationChannel(CHANNEL, "msnw", NotificationManager.IMPORTANCE_LOW))
        // Same intent as the launcher, so tapping the notification only brings
        // the existing activity (with its tabs) to the front.
        val launch = Intent(this, MainActivity::class.java).apply {
            action = Intent.ACTION_MAIN
            addCategory(Intent.CATEGORY_LAUNCHER)
            flags = Intent.FLAG_ACTIVITY_NEW_TASK
        }
        val open = PendingIntent.getActivity(this, 0, launch, PendingIntent.FLAG_IMMUTABLE)
        val n = synchronized(entries) { entries.count { !it.finished } }
        val text = if (n == 0) "proxy running" else "proxy running, $n mosh session${if (n == 1) "" else "s"}"
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_launcher)
            .setContentTitle("msnw")
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .build()
    }

    companion object {
        const val TAG = "msnw"
        const val CHANNEL = "msnw"
        const val PROXY_PORT = 8080
        const val ACTION_STOP = "ma.ikeji.msnw.STOP"
        const val ACTION_RESTART_PROXY = "ma.ikeji.msnw.RESTART_PROXY"

        fun start(ctx: Context, restartProxy: Boolean = false) {
            val i = Intent(ctx, MsnwService::class.java)
            if (restartProxy) i.action = ACTION_RESTART_PROXY
            ctx.startForegroundService(i)
        }
        fun logTail(ctx: Context, lines: Int = 40): String {
            val f = File(ctx.filesDir, "msnw.log")
            if (!f.exists()) return "(no log yet)"
            return f.readLines().takeLast(lines).joinToString("\n")
        }
    }
}
