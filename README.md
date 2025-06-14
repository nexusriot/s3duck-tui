S3Duck-TUI 🦆
======

TUI implementation of [S3Duck](https://github.com/nexusriot/s3duck)

Features
-------------

1. Profiles (create/edit/delete/clone) support
2. Walking buckets
3. Downloading support
4. Buckets/Objects deleting support
5. Bucket creating support
6. Upload support with simple local FS browser
7. FreeBSD support
------------- 

![Profiles](resources/00-profiles.png)

![Buckets](resources/01-create_profile.png)

![Folders](resources/02-bucket_list.png)

![Folders](resources/03-create_folder.png)

![Folders](resources/04-download.png)

![Folders](resources/05-local_browse.png)

![Folders](resources/06-upload.png)

Building
------------- 
```
go build
```
build statically without dependency on libc:
```
go build -ldflags "-linkmode external -extldflags -static"
```

Building deb package
------------- 
Install required packages:
```
sudo apt-get install git devscripts build-essential lintian upx-ucl
```
Run build:
```
./build-deb.sh
```
Building FreeBSD binary
------------- 
```
GOOS=freebsd GOARCH=amd64 go build
```