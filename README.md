+ 问题整理 https://blog.witd.in/2019/12/10/%E7%BD%91%E5%8D%A1%E9%A9%B1%E5%8A%A8%E5%8D%87%E7%BA%A7%E5%AF%BC%E8%87%B4%E5%AE%B9%E5%99%A8%E7%BD%91%E7%BB%9C%E8%AE%BE%E5%A4%87%E4%B8%A2%E5%A4%B1%E9%97%AE%E9%A2%98/ 

+ ```sh build.sh```  会产生ELF 版本 i40e_net_fix 文件

升级步骤
1. 停止kubelet
```systemctl stop kubelet```

2. 执行检查，防止有容器没有对应的checkpoint 文件
```cd /root && wget 1.1.1.1:8008/i40e_net_fix && chmod u+x i40e_net_fix && ./i40e_net_fix```

3. 执行升级驱动命令 
```
 cd  /tmp/i40e_fix/ && sh i40e_fix.sh
```
4. 执行修复
```cd /root && ./i40e_net_fix -fix=true ```

5. 检查容器内eth0 信息

``` 
docker ps |grep pause |awk '{print $1}' | while read ns
do 
    ip netns exec $ns ifconfig  # 检查eth0的ip
    ip netns exec $ns route  # 检查是否有路由信息
    ip netns exec $ns ping -c4 xxxx.com # 检查网络连通性
done
```
6. 启动kubelet
```systemctl start kubelet```
