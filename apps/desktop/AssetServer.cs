using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Reflection;
using System.Text;

namespace StarStack.Desktop;

/// <summary>
/// Serves the embedded React build on http://127.0.0.1:&lt;random&gt;/.
/// A loopback HTTP origin is deliberate: it is a secure context for the WebView,
/// it keeps the broker's Bearer token in normal localStorage, and it avoids the
/// mixed-content rules that would block an https://appassets host from calling
/// the http broker directly. HttpListener needs a URL ACL on Windows, so this is
/// a tiny HTTP/1.1 server over TcpListener instead.
/// </summary>
public sealed class AssetServer : IAsyncDisposable
{
    private static readonly Dictionary<string, string> Mime = new(StringComparer.OrdinalIgnoreCase)
    {
        [".html"] = "text/html; charset=utf-8",
        [".js"] = "text/javascript; charset=utf-8",
        [".mjs"] = "text/javascript; charset=utf-8",
        [".css"] = "text/css; charset=utf-8",
        [".json"] = "application/json; charset=utf-8",
        [".svg"] = "image/svg+xml",
        [".png"] = "image/png",
        [".jpg"] = "image/jpeg",
        [".jpeg"] = "image/jpeg",
        [".ico"] = "image/x-icon",
        [".woff"] = "font/woff",
        [".woff2"] = "font/woff2",
        [".ttf"] = "font/ttf",
        [".map"] = "application/json; charset=utf-8",
    };

    // A stable origin is what lets the WebView keep its localStorage: the broker
    // token lives there, and a random port would mean a fresh login on every launch.
    private const int PreferredPort = 17878;
    private TcpListener? _listener;
    private readonly Dictionary<string, byte[]> _assets = new(StringComparer.Ordinal);
    private CancellationTokenSource? _cts;
    private Task? _loop;

    public int Port { get; private set; }

    public int Start()
    {
        var assembly = Assembly.GetExecutingAssembly();
        foreach (var name in assembly.GetManifestResourceNames())
        {
            if (!name.StartsWith("web/", StringComparison.Ordinal)) continue;
            using var stream = assembly.GetManifestResourceStream(name);
            if (stream is null) continue;
            using var buffer = new MemoryStream();
            stream.CopyTo(buffer);
            // %(RecursiveDir) keeps the platform separator; the WebView asks for
            // forward-slash paths, so normalise the key once at startup.
            _assets[name["web/".Length..].Replace('\\', '/')] = buffer.ToArray();
        }
        if (_assets.Count == 0)
        {
            throw new InvalidOperationException("内嵌网页资源为空：请先在 apps/ 下执行 npm run build。");
        }

        try
        {
            _listener = new TcpListener(IPAddress.Loopback, PreferredPort);
            _listener.Start();
        }
        catch (SocketException)
        {
            // Port taken by something else: correctness first, session persistence second.
            _listener = new TcpListener(IPAddress.Loopback, 0);
            _listener.Start();
        }
        Port = ((IPEndPoint)_listener.LocalEndpoint).Port;
        _cts = new CancellationTokenSource();
        _loop = Task.Run(() => AcceptLoopAsync(_cts.Token));
        return Port;
    }

    private async Task AcceptLoopAsync(CancellationToken ct)
    {
        var listener = _listener ?? throw new InvalidOperationException("asset server not started");
        while (!ct.IsCancellationRequested)
        {
            TcpClient client;
            try
            {
                client = await listener.AcceptTcpClientAsync(ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                break;
            }
            catch (ObjectDisposedException)
            {
                break;
            }
            catch (SocketException)
            {
                continue;
            }
            _ = HandleAsync(client, ct);
        }
    }

    private async Task HandleAsync(TcpClient client, CancellationToken ct)
    {
        using (client)
        {
            try
            {
                client.NoDelay = true;
                using var stream = client.GetStream();
                using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
                timeout.CancelAfter(TimeSpan.FromSeconds(15));

                var request = await ReadRequestAsync(stream, timeout.Token).ConfigureAwait(false);
                if (request is null) return;
                var (method, target) = request.Value;

                var path = target.Split('?')[0];
                if (path.Length == 0 || path == "/") path = "/index.html";
                path = Uri.UnescapeDataString(path).TrimStart('/');
                if (path.Contains("..", StringComparison.Ordinal))
                {
                    await WriteAsync(stream, 400, "text/plain; charset=utf-8", Encoding.UTF8.GetBytes("bad path"), method, timeout.Token).ConfigureAwait(false);
                    return;
                }

                if (!_assets.TryGetValue(path, out var body))
                {
                    await WriteAsync(stream, 404, "text/plain; charset=utf-8", Encoding.UTF8.GetBytes("not found: " + path), method, timeout.Token).ConfigureAwait(false);
                    return;
                }

                var mime = Mime.TryGetValue(Path.GetExtension(path), out var known) ? known : "application/octet-stream";
                await WriteAsync(stream, 200, mime, body, method, timeout.Token).ConfigureAwait(false);
            }
            catch (Exception)
            {
                // A dropped connection from the WebView is routine; never crash the UI thread.
            }
        }
    }

    private static async Task<(string Method, string Target)?> ReadRequestAsync(NetworkStream stream, CancellationToken ct)
    {
        var buffer = new byte[8192];
        var used = 0;
        while (used < buffer.Length)
        {
            var read = await stream.ReadAsync(buffer.AsMemory(used), ct).ConfigureAwait(false);
            if (read <= 0) return null;
            used += read;
            var text = Encoding.ASCII.GetString(buffer, 0, used);
            var lineEnd = text.IndexOf("\r\n", StringComparison.Ordinal);
            if (lineEnd < 0) continue;
            var parts = text[..lineEnd].Split(' ');
            if (parts.Length < 2) return null;
            return (parts[0], parts[1]);
        }
        return null;
    }

    private static async Task WriteAsync(NetworkStream stream, int status, string contentType, byte[] body, string method, CancellationToken ct)
    {
        var reason = status switch
        {
            200 => "OK",
            400 => "Bad Request",
            404 => "Not Found",
            _ => "Error",
        };
        var head = new StringBuilder()
            .Append("HTTP/1.1 ").Append(status).Append(' ').Append(reason).Append("\r\n")
            .Append("Content-Type: ").Append(contentType).Append("\r\n")
            .Append("Content-Length: ").Append(body.Length).Append("\r\n")
            .Append("Cache-Control: no-store\r\n")
            .Append("X-Content-Type-Options: nosniff\r\n")
            .Append("Connection: close\r\n\r\n")
            .ToString();
        var headBytes = Encoding.ASCII.GetBytes(head);
        await stream.WriteAsync(headBytes, ct).ConfigureAwait(false);
        if (!string.Equals(method, "HEAD", StringComparison.OrdinalIgnoreCase))
        {
            await stream.WriteAsync(body, ct).ConfigureAwait(false);
        }
        await stream.FlushAsync(ct).ConfigureAwait(false);
    }

    public async ValueTask DisposeAsync()
    {
        try
        {
            _cts?.Cancel();
            _listener?.Stop();
            if (_loop is not null) await _loop.ConfigureAwait(false);
        }
        catch
        {
            // shutting down is best-effort
        }
        finally
        {
            _cts?.Dispose();
        }
    }
}
