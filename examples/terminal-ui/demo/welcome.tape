Output ../../website/static/img/terminal-ui-demo.gif
Output dist/terminal-ui-demo.mp4

Set Shell "zsh"
Set FontFamily "Menlo"
Set FontSize 16
Set LetterSpacing 0
Set LineHeight 1.2
Set Width 1440
Set Height 900
Set Theme "TokyoNightStorm"
Set TypingSpeed 35ms
Set Framerate 30
Set PlaybackSpeed 1

Env TERM "xterm-256color"
Env COLORTERM "truecolor"
Env NO_COLOR ""
Env PATH "./bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

Type "mango-tui --demo"
Enter
Sleep 3s

# Browse the discovered endpoints, then return to the built-in demo.
Up@250ms 2
Sleep 1s
Down@250ms 2
Sleep 1s

# Preview manual endpoint entry without changing the selected demo target.
Type "e"
Sleep 1s
Type "https://mango.example.com"
Sleep 2s
Escape
Sleep 1s

# Connect, use the inline fuzzy filter, then inspect a durable Session.
Enter
Sleep 3s
Type "/"
Sleep 1s
Type "launch"
Sleep 1s
Enter
Sleep 2s
Escape
Sleep 1s
Down
Sleep 2s
Enter
Sleep 4s

Escape
Sleep 2s
Tab@350ms 2
Down
Sleep 1s
Enter
Sleep 3s
Escape
Sleep 1s

Ctrl+C
Sleep 1s
Ctrl+C
Sleep 1s
