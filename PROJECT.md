Our objective is to have a webapp that can synchronize on the NAS a local dyrectory like it happens in Dropbox. However, we have some hard requirements:

1. The complete synchronization must happen only when the computer is connected to the same LAN and must not pass throught internet at all, only on the NAS network
2. All the automatic synchronization must happen at diff level, so that multiple files rewritten constantly only send data on internet that is minimized as much as possible.
3. The list of file is updated constantly. Also here, try to minimize the data sent through internet.
4. It is possible to sync individual files when we are online. In that case, we can actually trigger a sync (always by diff, except in the case of conflicts indeed) when accessing the file content.
5. The application has a setting including a possible selective synchornization, where it is possible to exclude folders both local and from the online source to be traced and synced at all.
6. It must be as lightweight as possible: no hurdle for the CPU, and written so that it does not burn through CPU usage for long time, even when multiple small files are written and modified.
7. To minimize compute and transfer, the edits can be merged into chunks: if 1 computer is synching constantly, you can have it group the edits into one single chunk for the diff. The information on which computers are synching is stored on the server, and the data collected there can be optimized to allow an easier synchronization to other PC, as the server keeps tracks to the latest synchronization for that PC. be aware of different clocks hours in different PC.
8. In case of conflicts warn the user, and you can have a dialog for him to ask which version of the file to keep local and on the server. The user might choose to keep two version idfferent local and on server, in this case, the synchronization for that file is stopped in the local.
