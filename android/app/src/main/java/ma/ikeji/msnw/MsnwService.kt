package ma.ikeji.msnw

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import java.io.File

/**
 * Foreground service that keeps "msnw client --http-proxy" running for the
 * browser tab. It restarts the process if it exits and holds a partial wake
 * lock so tunnels survive the screen turning off.
 */
class MsnwService : Service() {
    private var proc: Process? = null
    private var thread: Thread? = null
    @Volatile private var stopping = false
    private var wakeLock: PowerManager.WakeLock? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }
        startForeground(1, notification("msnw proxy running"))
        if (wakeLock == null) {
            wakeLock = (getSystemService(Context.POWER_SERVICE) as PowerManager)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "msnw:proxy").also { it.acquire() }
        }
        restart()
        return START_STICKY
    }

    /** (Re)starts the msnw process with the current settings. */
    private fun restart() {
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
        wakeLock?.let { if (it.isHeld) it.release() }
        super.onDestroy()
    }

    private fun notification(text: String): Notification {
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.createNotificationChannel(NotificationChannel(CHANNEL, "msnw", NotificationManager.IMPORTANCE_LOW))
        // Same intent as the launcher, so tapping the notification only brings
        // the existing activity (with its tabs and sessions) to the front.
        val launch = Intent(this, MainActivity::class.java).apply {
            action = Intent.ACTION_MAIN
            addCategory(Intent.CATEGORY_LAUNCHER)
            flags = Intent.FLAG_ACTIVITY_NEW_TASK
        }
        val open = PendingIntent.getActivity(this, 0, launch, PendingIntent.FLAG_IMMUTABLE)
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

        fun start(ctx: Context) = ctx.startForegroundService(Intent(ctx, MsnwService::class.java))
        fun logTail(ctx: Context, lines: Int = 40): String {
            val f = File(ctx.filesDir, "msnw.log")
            if (!f.exists()) return "(no log yet)"
            return f.readLines().takeLast(lines).joinToString("\n")
        }
    }
}
