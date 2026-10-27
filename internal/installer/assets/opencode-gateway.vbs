Set sh = CreateObject("WScript.Shell")
home = "@@HOME@@"
sh.Run """" & home & "\app\gateway.exe"" --open-admin --home """ & home & """", 0, False
