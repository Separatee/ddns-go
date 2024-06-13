package dns

import (
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/jeessy2/ddns-go/config"
    "github.com/jeessy2/ddns-go/util"
)

const (
    zonesAPI = "https://api.cloudflare.com/client/v4/zones"
)

type Cloudflare struct {
    DNSConfig
    Domains config.Domains
}

type CloudflareResponse struct {
    Success  bool                   `json:"success"`
    Messages []string               `json:"messages"`
    Errors   []CloudflareError      `json:"errors"`
    Result   []CloudflareZoneResult `json:"result"`
}

type CloudflareRecordsResp struct {
    Success  bool                     `json:"success"`
    Messages []string                 `json:"messages"`
    Errors   []CloudflareError        `json:"errors"`
    Result   []CloudflareRecordResult `json:"result"`
}

type CloudflareError struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}

type CloudflareZoneResult struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

type CloudflareRecordResult struct {
    ID        string `json:"id"`
    Type      string `json:"type"`
    Name      string `json:"name"`
    Content   string `json:"content"`
    Proxied   bool   `json:"proxied"`
    CreatedOn string `json:"created_on"`
    ModifiedOn string `json:"modified_on"`
}

func NewCloudflare(dnsConfig DNSConfig) *Cloudflare {
    return &Cloudflare{DNSConfig: dnsConfig}
}

func (cf *Cloudflare) AddUpdateDomainRecords() config.Domains {
    cf.addUpdateDomainRecords("A")
    cf.addUpdateDomainRecords("AAAA")
    return cf.Domains
}

func (cf *Cloudflare) addUpdateDomainRecords(recordType string) {
    ipAddr, domains := cf.Domains.GetNewIpResult(recordType)
    if ipAddr == "" {
        return
    }

    for _, domain := range domains {
        // get zone
        result, err := cf.getZones(domain)
        if err != nil {
            util.Log("查询域名信息发生异常! %s", err)
            domain.UpdateStatus = config.UpdatedFailed
            continue
        }
        if len(result.Result) == 0 {
            util.Log("在DNS服务商中未找到根域名: %s", domain.DomainName)
            domain.UpdateStatus = config.UpdatedFailed
            continue
        }

        zoneID := result.Result[0].ID
        var records CloudflareRecordsResp
        // 获取现有记录
        err = cf.request(
            "GET",
            fmt.Sprintf(zonesAPI+"/%s/dns_records?type=%s&name=%s&per_page=50", zoneID, recordType, domain.GetSubDomain()+"."+domain.GetTopDomain()),
            nil, &records,
        )
        if err != nil {
            util.Log("查询域名信息发生异常! %s", err)
            domain.UpdateStatus = config.UpdatedFailed
            continue
        }
        if !records.Success {
            util.Log("查询域名信息发生异常! %s", strings.Join(records.Messages, ", "))
            domain.UpdateStatus = config.UpdatedFailed
            continue
        }

        // 根据记录存在与否决定添加或更新
        if len(records.Result) > 0 {
            cf.modify(records, zoneID, domain, ipAddr)
        } else {
            cf.create(zoneID, domain, recordType, ipAddr)
        }

        // 清理多余的相同解析记录
        cf.cleanDuplicateRecords(zoneID, recordType, domain, records)
    }
}

func (cf *Cloudflare) getZones(domain config.Domain) (*CloudflareResponse, error) {
    var result CloudflareResponse
    err := cf.request("GET", zonesAPI+"?name="+domain.GetTopDomain(), nil, &result)
    return &result, err
}

func (cf *Cloudflare) create(zoneID string, domain config.Domain, recordType, ipAddr string) {
    record := map[string]interface{}{
        "type":    recordType,
        "name":    domain.GetSubDomain(),
        "content": ipAddr,
        "ttl":     cf.TTL,
        "proxied": cf.Proxy,
    }

    var result CloudflareResponse
    err := cf.request("POST", fmt.Sprintf(zonesAPI+"/%s/dns_records", zoneID), record, &result)
    if err != nil || !result.Success {
        util.Log("添加DNS记录失败! %s", strings.Join(result.Messages, ", "))
        domain.UpdateStatus = config.UpdatedFailed
    } else {
        util.Log("添加DNS记录成功!")
        domain.UpdateStatus = config.UpdatedSuccess
    }
}

func (cf *Cloudflare) modify(records CloudflareRecordsResp, zoneID string, domain config.Domain, ipAddr string) {
    record := map[string]interface{}{
        "type":    records.Result[0].Type,
        "name":    records.Result[0].Name,
        "content": ipAddr,
        "ttl":     cf.TTL,
        "proxied": records.Result[0].Proxied,
    }

    var result CloudflareResponse
    err := cf.request("PUT", fmt.Sprintf(zonesAPI+"/%s/dns_records/%s", zoneID, records.Result[0].ID), record, &result)
    if err != nil || !result.Success {
        util.Log("更新DNS记录失败! %s", strings.Join(result.Messages, ", "))
        domain.UpdateStatus = config.UpdatedFailed
    } else {
        util.Log("更新DNS记录成功!")
        domain.UpdateStatus = config.UpdatedSuccess
    }
}

func (cf *Cloudflare) cleanDuplicateRecords(zoneID, recordType string, domain config.Domain, records CloudflareRecordsResp) {
    // 获取最新的解析记录ID
    var latestRecordID string
    latestTime := time.Time{}
    for _, record := range records.Result {
        // 比较解析记录的创建时间或修改时间，找到最新的记录
        recordTime, err := time.Parse(time.RFC3339, record.CreatedOn)
        if err != nil {
            recordTime, err = time.Parse(time.RFC3339, record.ModifiedOn)
            if err != nil {
                continue
            }
        }
        if recordTime.After(latestTime) {
            latestTime = recordTime
            latestRecordID = record.ID
        }
    }

    // 删除多余的相同解析记录
    for _, record := range records.Result {
        if record.ID != latestRecordID {
            var result CloudflareResponse
            err := cf.request("DELETE", fmt.Sprintf(zonesAPI+"/%s/dns_records/%s", zoneID, record.ID), nil, &result)
            if err != nil || !result.Success {
                util.Log("删除多余DNS记录失败! %s", strings.Join(result.Messages, ", "))
            } else {
                util.Log("删除多余DNS记录成功!")
            }
        }
    }
}

func (cf *Cloudflare) request(method, url string, body interface{}, result interface{}) error {
    client := &http.Client{
        Timeout: time.Second * 30,
    }
    req, err := util.NewJSONRequest(method, url, body)
    if err != nil {
        return err
    }

    req.Header.Set("Authorization", "Bearer "+cf.DNSConfig.Secret)
    req.Header.Set("Content-Type", "application/json")

    resp, err := client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    return util.ParseJSONResponse(resp.Body, result)
}
