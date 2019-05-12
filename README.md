## 定时任务
1. 支持创建定时计划
```
c := cron.New()
spec := "* * * * *"
jobName := "testFunc"

c.AddFunc(spec, jobName, func() {
	fmt.Println("This is a cron func")
})

c.AddJob(spec, jobName, job)

c.Run()
```
2. 支持动态更新定时计划
```
c := cron.New()
spec := "* * * * *"
jobName := "testFunc"

c.AddFunc(spec, jobName, func() {
	fmt.Println("This is a cron func")
})
go c.Run()

time.Sleep(time.Second*5)

c.UpdateFunc(spec, jobName, func() {
	fmt.Println("This is a cron funcB")
})

time.Sleep(time.Second*5)
```
3. 支持动态删除定时计划
```
c := cron.New()
spec := "* * * * *"
jobName := "testFunc"

c.AddFunc(spec, jobName, func() {
	fmt.Println("This is a cron func")
})
go c.Run()

time.Sleep(time.Second*5)

c.RemoveJobOrFunc(jobName)

time.Sleep(time.Second*5)
```