using System.Threading;
using System.Windows;

namespace StarStack.Desktop;

public partial class App : Application
{
    private Mutex? _single;

    protected override void OnStartup(StartupEventArgs e)
    {
        _single = new Mutex(true, "StarStack.Desktop.SingleInstance", out var created);
        if (!created)
        {
            MessageBox.Show("星栈算力已经在运行。", "星栈算力", MessageBoxButton.OK, MessageBoxImage.Information);
            Shutdown();
            return;
        }
        base.OnStartup(e);
    }

    protected override void OnExit(ExitEventArgs e)
    {
        _single?.Dispose();
        base.OnExit(e);
    }
}
