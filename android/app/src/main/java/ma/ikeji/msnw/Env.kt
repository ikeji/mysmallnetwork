package ma.ikeji.msnw

import android.content.Context
import android.system.Os
import android.util.Log
import java.io.File
import java.net.Inet6Address
import java.net.InetAddress

/**
 * Lays out the app's private "prefix": a bin directory of symlinks to the
 * bundled executables (which Android extracted into nativeLibraryDir), the
 * terminfo database, a home directory, and the environment the processes run
 * with. Symlinks are used because only files under nativeLibraryDir may be
 * executed, while "msnw mosh" looks up "ssh" and "mosh-client" by name on PATH.
 */
object Env {
    private const val PREFS = "msnw"

    fun prefs(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    fun bin(ctx: Context): File = File(ctx.filesDir, "bin")
    fun home(ctx: Context): File = File(ctx.filesDir, "home")
    fun terminfo(ctx: Context): File = File(ctx.filesDir, "terminfo")
    fun msnw(ctx: Context): File = File(bin(ctx), "msnw")
    fun logFile(ctx: Context): File = File(ctx.filesDir, "msnw.log")
    fun sshKey(ctx: Context): File = File(home(ctx), ".ssh/id_msnw")

    /** Newline-separated most-recent-first history lists kept in the prefs. */
    fun history(ctx: Context, key: String): List<String> =
        prefs(ctx).getString(key, "")?.split('\n')?.filter { it.isNotBlank() } ?: emptyList()

    fun addHistory(ctx: Context, key: String, value: String) {
        val v = value.trim()
        if (v.isEmpty()) return
        val list = listOf(v) + history(ctx, key).filter { it != v }
        prefs(ctx).edit().putString(key, list.take(20).joinToString("\n")).apply()
    }

    fun removeHistory(ctx: Context, key: String, value: String) {
        prefs(ctx).edit().putString(key, history(ctx, key).filter { it != value }.joinToString("\n")).apply()
    }

    /**
     * Generates the ssh key pair (ed25519, dropbear format) if it does not
     * exist and returns the public key line to add to authorized_keys on the
     * exporter host. Blocking: runs dropbearkey.
     */
    fun ensureSshKey(ctx: Context): String {
        val key = sshKey(ctx)
        val bin = bin(ctx)
        if (!key.exists()) {
            key.parentFile?.mkdirs()
            val gen = ProcessBuilder(File(bin, "dropbearkey").absolutePath, "-t", "ed25519", "-f", key.absolutePath)
                .redirectErrorStream(true).start()
            val out = gen.inputStream.bufferedReader().readText()
            if (gen.waitFor() != 0) throw RuntimeException("dropbearkey failed: $out")
        }
        val show = ProcessBuilder(File(bin, "dropbearkey").absolutePath, "-y", "-f", key.absolutePath)
            .redirectErrorStream(true).start()
        val out = show.inputStream.bufferedReader().readText()
        show.waitFor()
        return out.lines().firstOrNull { it.startsWith("ssh-") } ?: throw RuntimeException("no public key in: $out")
    }

    /** Creates directories and symlinks; safe to call on every start. */
    fun setup(ctx: Context) {
        val lib = File(ctx.applicationInfo.nativeLibraryDir)
        val bin = bin(ctx)
        bin.mkdirs(); home(ctx).mkdirs(); File(home(ctx), ".ssh").mkdirs()
        mapOf(
            "msnw" to "libmsnw.so",
            "mosh-client" to "libmosh-client.so",
            "dbclient" to "libdbclient.so",
            "dropbearkey" to "libdropbearkey.so",
            "ssh" to "libssh.so",
        ).forEach { (name, lib_) ->
            val link = File(bin, name)
            link.delete()
            Os.symlink(File(lib, lib_).absolutePath, link.absolutePath)
        }
        copyAssets(ctx, "terminfo", terminfo(ctx))
    }

    private fun copyAssets(ctx: Context, path: String, dst: File) {
        val entries = ctx.assets.list(path) ?: return
        if (entries.isEmpty()) { // a file
            dst.parentFile?.mkdirs()
            ctx.assets.open(path).use { i -> dst.outputStream().use { o -> i.copyTo(o) } }
            return
        }
        dst.mkdirs()
        entries.forEach { copyAssets(ctx, "$path/$it", File(dst, it)) }
    }

    const val DEFAULT_SERVER = "relay.ikeji.ma:4433"

    /**
     * The rendezvous server as "ip:port". msnw is a static Go binary whose
     * resolver needs /etc/resolv.conf, which Android does not have, so the
     * name is resolved here with the system resolver. Blocking: call off the
     * main thread. Falls back to the name if resolution fails.
     */
    fun resolvedServer(ctx: Context): String {
        val configured = prefs(ctx).getString("server", "")?.takeIf { it.isNotBlank() } ?: DEFAULT_SERVER
        val i = configured.lastIndexOf(':')
        val host = if (i > 0 && !configured.startsWith("[")) configured.substring(0, i) else configured
        val port = if (i > 0) configured.substring(i + 1) else "4433"
        return try {
            val addrs = InetAddress.getAllByName(host)
            val a = addrs.firstOrNull { it !is Inet6Address } ?: addrs.first() // prefer IPv4: the relay speaks v4
            val ip = a.hostAddress ?: return configured
            if (a is Inet6Address) "[$ip]:$port" else "$ip:$port"
        } catch (e: Exception) {
            Log.w("msnw", "cannot resolve $host: $e")
            configured
        }
    }

    /** Environment for msnw and the programs it spawns. Blocking (DNS); call off the main thread. */
    fun environment(ctx: Context): Map<String, String> {
        val p = prefs(ctx)
        val env = linkedMapOf(
            "PATH" to bin(ctx).absolutePath + ":" + (System.getenv("PATH") ?: "/system/bin"),
            "HOME" to home(ctx).absolutePath,
            "TMPDIR" to ctx.cacheDir.absolutePath,
            "TERM" to "xterm-256color",
            "TERMINFO" to terminfo(ctx).absolutePath,
            "LANG" to "en_US.UTF-8",
            "MSNW_KEY" to (p.getString("key", "") ?: ""),
            "MSNW_DBCLIENT" to File(bin(ctx), "dbclient").absolutePath,
            "MSNW_LOG" to logFile(ctx).absolutePath,
        )
        env["MSNW_SERVER"] = resolvedServer(ctx)
        return env
    }

    fun envArray(ctx: Context): Array<String> = environment(ctx).map { "${it.key}=${it.value}" }.toTypedArray()
}
