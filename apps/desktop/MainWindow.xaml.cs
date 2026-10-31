using System.Diagnostics;
using System.IO;
using System.Text.Json;
using System.Windows;
using System.Windows.Input;
using Microsoft.Web.WebView2.Core;

namespace StarStack.Desktop;

public partial class MainWindow : Window
{
    private readonly AssetServer _server = new();
    private string _baseUrl = string.Empty;

    public MainWindow()
    {
        InitializeComponent();
        Loaded += OnLoaded;
        Closing += OnClosing;
        PreviewKeyDown += OnKeyDown;
    }

    private async void OnLoaded(object sender, RoutedEventArgs e)
    {
        ApplySavedBounds();
        try
        {
            var port = _server.Start();
            _baseUrl = $"http://127.0.0.1:{port}/";
            await InitWebViewAsync();
        }
        catch (Exception ex)
        {
            ShowError("桌面端启动失败：" + ex.Message);
        }
    }

    private async Task InitWebViewAsync()
    {
        var dataFolder = Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
            "StarStack", "WebView2");
        Directory.CreateDirectory(dataFolder);

        var environment = await CoreWebView2Environment.CreateAsync(null, dataFolder);
        await Web.EnsureCoreWebView2Async(environment);

        var core = Web.CoreWebView2;
        core.Settings.IsStatusBarEnabled = false;
        core.Settings.AreDevToolsEnabled = true;
        core.Settings.AreDefaultContextMenusEnabled = true;
        core.Settings.AreBrowserAcceleratorKeysEnabled = true;

        // Downloads and "open in new window" links belong to the system browser,
        // not to a second chrome-less window inside the app.
        core.NewWindowRequested += (_, args) =>
        {
            args.Handled = true;
            OpenExternal(args.Uri);
        };

        core.NavigationStarting += (_, args) =>
        {
            if (!args.Uri.StartsWith(_baseUrl, StringComparison.OrdinalIgnoreCase))
            {
                args.Cancel = true;
                OpenExternal(args.Uri);
            }
        };

        core.NavigationCompleted += (_, args) =>
        {
            if (args.IsSuccess)
            {
                Overlay.Visibility = Visibility.Collapsed;
            }
            else
            {
                ShowError("页面加载失败：" + args.WebErrorStatus);
            }
        };

        // Save artifacts straight to the user's Downloads folder. Without this the
        // WebView shows its own download flyout, which looks like a browser bolted
        // onto a desktop app.
        core.DownloadStarting += (_, args) =>
        {
            try
            {
                var downloads = Path.Combine(
                    Environment.GetFolderPath(Environment.SpecialFolder.UserProfile), "Downloads");
                Directory.CreateDirectory(downloads);
                args.ResultFilePath = Path.Combine(downloads, Path.GetFileName(args.ResultFilePath));
                args.Handled = true;
            }
            catch
            {
                // fall back to the WebView's own download UI
            }
        };

        core.Navigate(_baseUrl);
    }

    private static void OpenExternal(string uri)
    {
        try
        {
            Process.Start(new ProcessStartInfo(uri) { UseShellExecute = true });
        }
        catch
        {
            // no default browser registered; nothing useful to do here
        }
    }

    private void ShowError(string message)
    {
        ErrorText.Text = message;
        Overlay.Visibility = Visibility.Visible;
    }

    private void OnKeyDown(object sender, KeyEventArgs e)
    {
        if (e.Key == Key.F5)
        {
            Web.CoreWebView2?.Reload();
            e.Handled = true;
        }
        else if (e.Key == Key.F12)
        {
            Web.CoreWebView2?.OpenDevToolsWindow();
            e.Handled = true;
        }
    }

    private void Reload_Click(object sender, RoutedEventArgs e) => Web.CoreWebView2?.Reload();

    private void DevTools_Click(object sender, RoutedEventArgs e) => Web.CoreWebView2?.OpenDevToolsWindow();

    private void ResetZoom_Click(object sender, RoutedEventArgs e) => Web.ZoomFactor = 1.0;

    private void OpenWeb_Click(object sender, RoutedEventArgs e)
    {
        if (_baseUrl.Length > 0) OpenExternal(_baseUrl);
    }

    private void Exit_Click(object sender, RoutedEventArgs e) => Close();

    private void About_Click(object sender, RoutedEventArgs e)
    {
        MessageBox.Show(
            "星栈算力 桌面端 v1.0\n\n" +
            "React + WPF WebView2，控制台资源内嵌在程序里，不依赖远程网页。\n" +
            "账号、任务、算力市场与账单与网页端一致。\n\n" +
            $"本地控制台：{(_baseUrl.Length > 0 ? _baseUrl : "—")}",
            "关于星栈算力",
            MessageBoxButton.OK,
            MessageBoxImage.Information);
    }

    private static string BoundsPath => Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
        "StarStack", "window.json");

    public sealed class WindowBounds
    {
        public double Left { get; set; }
        public double Top { get; set; }
        public double Width { get; set; }
        public double Height { get; set; }
        public bool Maximized { get; set; }
    }

    private void ApplySavedBounds()
    {
        try
        {
            if (!File.Exists(BoundsPath)) return;
            var saved = JsonSerializer.Deserialize<WindowBounds>(File.ReadAllText(BoundsPath));
            if (saved is null) return;

            var virtualLeft = SystemParameters.VirtualScreenLeft;
            var virtualTop = SystemParameters.VirtualScreenTop;
            var virtualRight = virtualLeft + SystemParameters.VirtualScreenWidth;
            var virtualBottom = virtualTop + SystemParameters.VirtualScreenHeight;
            var visible = saved.Left < virtualRight - 80 && saved.Top < virtualBottom - 80
                          && saved.Left + saved.Width > virtualLeft + 80 && saved.Top + saved.Height > virtualTop + 80;
            if (visible && saved.Width >= MinWidth && saved.Height >= MinHeight)
            {
                Left = saved.Left;
                Top = saved.Top;
                Width = saved.Width;
                Height = saved.Height;
            }
            if (saved.Maximized) WindowState = WindowState.Maximized;
        }
        catch
        {
            // a corrupt bounds file must never block startup
        }
    }

    private void OnClosing(object? sender, System.ComponentModel.CancelEventArgs e)
    {
        try
        {
            var bounds = WindowState == WindowState.Normal ? new Rect(Left, Top, Width, Height) : RestoreBounds;
            Directory.CreateDirectory(Path.GetDirectoryName(BoundsPath)!);
            var saved = new WindowBounds
            {
                Left = bounds.Left,
                Top = bounds.Top,
                Width = bounds.Width,
                Height = bounds.Height,
                Maximized = WindowState == WindowState.Maximized,
            };
            File.WriteAllText(BoundsPath, JsonSerializer.Serialize(saved));
        }
        catch
        {
            // preference persistence is best-effort
        }

        _ = _server.DisposeAsync();
    }
}
