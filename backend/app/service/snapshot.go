package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/1Panel-dev/1Panel/backend/app/dto"
	"github.com/1Panel-dev/1Panel/backend/app/model"
	"github.com/1Panel-dev/1Panel/backend/constant"
	"github.com/1Panel-dev/1Panel/backend/global"
	"github.com/1Panel-dev/1Panel/backend/utils/cmd"
	"github.com/1Panel-dev/1Panel/backend/utils/docker"
	"github.com/1Panel-dev/1Panel/backend/utils/files"
	"github.com/jinzhu/copier"
	"github.com/pkg/errors"
)

type SnapshotService struct{}

type ISnapshotService interface {
	SearchWithPage(req dto.PageInfo) (int64, interface{}, error)
	SnapshotCreate(req dto.SnapshotCreate) error
	SnapshotRecover(req dto.SnapshotRecover) error
	SnapshotRollback(req dto.SnapshotRecover) error
	Delete(req dto.BatchDeleteReq) error

	readFromJson(path string) (SnapshotJson, error)
}

func NewISnapshotService() ISnapshotService {
	return &SnapshotService{}
}

func (u *SnapshotService) SearchWithPage(req dto.PageInfo) (int64, interface{}, error) {
	total, systemBackups, err := snapshotRepo.Page(req.Page, req.PageSize)
	var dtoSnap []dto.SnapshotInfo
	for _, systemBackup := range systemBackups {
		var item dto.SnapshotInfo
		if err := copier.Copy(&item, &systemBackup); err != nil {
			return 0, nil, errors.WithMessage(constant.ErrStructTransform, err.Error())
		}
		dtoSnap = append(dtoSnap, item)
	}
	return total, dtoSnap, err
}

type SnapshotJson struct {
	OldDockerDataDir string `json:"oldDockerDataDir"`
	OldBackupDataDir string `json:"oldDackupDataDir"`
	OldPanelDataDir  string `json:"oldPanelDataDir"`

	DockerDataDir      string `json:"dockerDataDir"`
	BackupDataDir      string `json:"backupDataDir"`
	PanelDataDir       string `json:"panelDataDir"`
	LiveRestoreEnabled bool   `json:"liveRestoreEnabled"`
}

func (u *SnapshotService) SnapshotCreate(req dto.SnapshotCreate) error {
	global.LOG.Info("start to create snapshot now")
	localDir, err := loadLocalDir()
	if err != nil {
		return err
	}
	backup, err := backupRepo.Get(commonRepo.WithByType(req.From))
	if err != nil {
		return err
	}
	backupAccont, err := NewIBackupService().NewClient(&backup)
	if err != nil {
		return err
	}

	timeNow := time.Now().Format("20060102150405")
	rootDir := fmt.Sprintf("%s/system/1panel_snapshot_%s", localDir, timeNow)
	backupPanelDir := fmt.Sprintf("%s/1panel", rootDir)
	_ = os.MkdirAll(backupPanelDir, os.ModePerm)
	backupDockerDir := fmt.Sprintf("%s/docker", rootDir)
	_ = os.MkdirAll(backupDockerDir, os.ModePerm)

	versionItem, _ := settingRepo.Get(settingRepo.WithByKey("SystemVersion"))
	snap := model.Snapshot{
		Name:        "1panel_snapshot_" + timeNow,
		Description: req.Description,
		From:        req.From,
		Version:     versionItem.Value,
		Status:      constant.StatusWaiting,
	}
	_ = snapshotRepo.Create(&snap)
	go func() {
		defer func() {
			global.LOG.Info("zhengque zoudao le zheli")
			_ = os.RemoveAll(rootDir)
		}()
		fileOp := files.NewFileOp()

		dockerDataDir, liveRestoreStatus, err := u.loadDockerDataDir()
		if err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}
		_, _ = cmd.Exec("systemctl stop docker")
		if err := u.handleDockerDatas(fileOp, "snapshot", dockerDataDir, backupDockerDir); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}
		if err := u.handleDaemonJson(fileOp, "snapshot", "", backupDockerDir); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}
		_, _ = cmd.Exec("systemctl restart docker")

		if err := u.handlePanelBinary(fileOp, "snapshot", "", backupPanelDir+"/1panel"); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}
		if err := u.handlePanelctlBinary(fileOp, "snapshot", "", backupPanelDir+"/1pctl"); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}
		if err := u.handlePanelService(fileOp, "snapshot", "", backupPanelDir+"/1panel.service"); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}

		if err := u.handleBackupDatas(fileOp, "snapshot", localDir, backupPanelDir); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}

		if err := u.handlePanelDatas(fileOp, "snapshot", global.CONF.BaseDir+"/1Panel", backupPanelDir, localDir, dockerDataDir); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}

		snapJson := SnapshotJson{DockerDataDir: dockerDataDir, BackupDataDir: localDir, PanelDataDir: global.CONF.BaseDir + "/1Panel", LiveRestoreEnabled: liveRestoreStatus}
		if err := u.saveJson(snapJson, rootDir); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, fmt.Sprintf("save snapshot json failed, err: %v", err))
			return
		}

		if err := fileOp.Compress([]string{rootDir}, fmt.Sprintf("%s/system", localDir), fmt.Sprintf("1panel_snapshot_%s.tar.gz", timeNow), files.TarGz); err != nil {
			updateSnapshotStatus(snap.ID, constant.StatusFailed, err.Error())
			return
		}
		global.LOG.Infof("start to upload snapshot to %s, please wait", backup.Type)
		localPath := fmt.Sprintf("%s/system/1panel_snapshot_%s.tar.gz", localDir, timeNow)
		if ok, err := backupAccont.Upload(localPath, fmt.Sprintf("system_snapshot/1panel_snapshot_%s.tar.gz", timeNow)); err != nil || !ok {
			_ = snapshotRepo.Update(snap.ID, map[string]interface{}{"status": constant.StatusFailed, "message": err.Error()})
			global.LOG.Errorf("upload snapshot to %s failed, err: %v", backup.Type, err)
			return
		}
		_ = snapshotRepo.Update(snap.ID, map[string]interface{}{"status": constant.StatusSuccess})
		_ = os.RemoveAll(rootDir)
		_ = os.RemoveAll(fmt.Sprintf("%s/system/1panel_snapshot_%s.tar.gz", localDir, timeNow))

		updateSnapshotStatus(snap.ID, constant.StatusSuccess, "")
		global.LOG.Infof("upload snapshot to %s success", backup.Type)
	}()
	return nil
}

func (u *SnapshotService) SnapshotRecover(req dto.SnapshotRecover) error {
	global.LOG.Info("start to recvover panel by snapshot now")
	snap, err := snapshotRepo.Get(commonRepo.WithByID(req.ID))
	if err != nil {
		return err
	}
	if !req.IsNew && len(snap.InterruptStep) != 0 && len(snap.RollbackStatus) != 0 {
		return fmt.Errorf("the snapshot has been rolled back and cannot be restored again")
	}
	isReTry := false
	if len(snap.InterruptStep) != 0 && !req.IsNew {
		isReTry = true
	}
	backup, err := backupRepo.Get(commonRepo.WithByType(snap.From))
	if err != nil {
		return err
	}
	client, err := NewIBackupService().NewClient(&backup)
	if err != nil {
		return err
	}
	localDir, err := loadLocalDir()
	if err != nil {
		return err
	}
	baseDir := fmt.Sprintf("%s/system/%s", localDir, snap.Name)
	if _, err := os.Stat(baseDir); err != nil && os.IsNotExist(err) {
		_ = os.MkdirAll(baseDir, os.ModePerm)
	}

	_ = snapshotRepo.Update(snap.ID, map[string]interface{}{"recover_status": constant.StatusWaiting})
	go func() {
		operation := "recover"
		if isReTry {
			operation = "re-recover"
		}
		if !isReTry || snap.InterruptStep == "Download" || (isReTry && req.ReDownload) {
			ok, err := client.Download(fmt.Sprintf("system_snapshot/%s.tar.gz", snap.Name), fmt.Sprintf("%s/%s.tar.gz", baseDir, snap.Name))
			if err != nil || !ok {
				if req.ReDownload {
					updateRecoverStatus(snap.ID, snap.InterruptStep, constant.StatusFailed, fmt.Sprintf("download file %s from %s failed, err: %v", snap.Name, backup.Type, err))
					return
				}
				updateRecoverStatus(snap.ID, "Download", constant.StatusFailed, fmt.Sprintf("download file %s from %s failed, err: %v", snap.Name, backup.Type, err))
				return
			}
			isReTry = false
		}
		fileOp := files.NewFileOp()
		if !isReTry || snap.InterruptStep == "Decompress" || (isReTry && req.ReDownload) {
			if err := fileOp.Decompress(fmt.Sprintf("%s/%s.tar.gz", baseDir, snap.Name), baseDir, files.TarGz); err != nil {
				if req.ReDownload {
					updateRecoverStatus(snap.ID, snap.InterruptStep, constant.StatusFailed, fmt.Sprintf("decompress file failed, err: %v", err))
					return
				}
				updateRecoverStatus(snap.ID, "Decompress", constant.StatusFailed, fmt.Sprintf("decompress file failed, err: %v", err))
				return
			}
			isReTry = false
		}
		rootDir := fmt.Sprintf("%s/%s", baseDir, snap.Name)
		originalDir := fmt.Sprintf("%s/original/", baseDir)

		snapJson, err := u.readFromJson(fmt.Sprintf("%s/snapshot.json", rootDir))
		if err != nil {
			updateRecoverStatus(snap.ID, "Readjson", constant.StatusFailed, fmt.Sprintf("decompress file failed, err: %v", err))
			return
		}
		if snap.InterruptStep == "Readjson" {
			isReTry = false
		}

		snapJson.OldPanelDataDir = global.CONF.BaseDir + "/1Panel"
		snapJson.OldBackupDataDir = localDir
		recoverPanelDir := fmt.Sprintf("%s/%s/1panel", baseDir, snap.Name)
		liveRestore := false
		if !isReTry || snap.InterruptStep == "LoadDockerJson" {
			snapJson.OldDockerDataDir, liveRestore, err = u.loadDockerDataDir()
			if err != nil {
				updateRecoverStatus(snap.ID, "LoadDockerJson", constant.StatusFailed, fmt.Sprintf("load docker data dir failed, err: %v", err))
				return
			}
			isReTry = false
		}
		if liveRestore {
			if err := u.updateLiveRestore(false); err != nil {
				updateRecoverStatus(snap.ID, "UpdateLiveRestore", constant.StatusFailed, fmt.Sprintf("update docker daemon.json live-restore conf failed, err: %v", err))
				return
			}
			isReTry = false
		}
		_ = u.saveJson(snapJson, rootDir)

		_, _ = cmd.Exec("systemctl stop docker")
		if !isReTry || snap.InterruptStep == "DockerDir" {
			if err := u.handleDockerDatas(fileOp, operation, rootDir, snapJson.DockerDataDir); err != nil {
				updateRecoverStatus(snap.ID, "DockerDir", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}
		if !isReTry || snap.InterruptStep == "DaemonJson" {
			if err := u.handleDaemonJson(fileOp, operation, rootDir+"/docker/daemon.json", originalDir); err != nil {
				updateRecoverStatus(snap.ID, "DaemonJson", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}
		_, _ = cmd.Exec("systemctl restart docker")

		if !isReTry || snap.InterruptStep == "1PanelBinary" {
			if err := u.handlePanelBinary(fileOp, operation, recoverPanelDir+"/1panel", originalDir+"/1panel"); err != nil {
				updateRecoverStatus(snap.ID, "1PanelBinary", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}
		if !isReTry || snap.InterruptStep == "1PctlBinary" {
			if err := u.handlePanelctlBinary(fileOp, operation, recoverPanelDir+"/1pctl", originalDir+"/1pctl"); err != nil {
				updateRecoverStatus(snap.ID, "1PctlBinary", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}
		if !isReTry || snap.InterruptStep == "1PanelService" {
			if err := u.handlePanelService(fileOp, operation, recoverPanelDir+"/1panel.service", originalDir+"/1panel.service"); err != nil {
				updateRecoverStatus(snap.ID, "1PanelService", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}

		if !isReTry || snap.InterruptStep == "1PanelBackups" {
			if err := u.handleBackupDatas(fileOp, operation, rootDir, snapJson.BackupDataDir); err != nil {
				updateRecoverStatus(snap.ID, "1PanelBackups", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}

		if !isReTry || snap.InterruptStep == "1PanelData" {
			if err := u.handlePanelDatas(fileOp, operation, rootDir, snapJson.PanelDataDir, "", ""); err != nil {
				updateRecoverStatus(snap.ID, "1PanelData", constant.StatusFailed, err.Error())
				return
			}
			isReTry = false
		}
		fmt.Println(000)
		_ = os.RemoveAll(rootDir)
		fmt.Println(111)
		global.LOG.Info("recover successful")
		fmt.Println(222)
		_, _ = cmd.Exec("systemctl daemon-reload")
		fmt.Println(333)
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		fmt.Println(444)
		updateRecoverStatus(snap.ID, "", constant.StatusSuccess, "")
		fmt.Println(555)
	}()
	return nil
}

func (u *SnapshotService) SnapshotRollback(req dto.SnapshotRecover) error {
	global.LOG.Info("start to rollback now")
	snap, err := snapshotRepo.Get(commonRepo.WithByID(req.ID))
	if err != nil {
		return err
	}
	if snap.InterruptStep == "Download" || snap.InterruptStep == "Decompress" || snap.InterruptStep == "Readjson" {
		return nil
	}
	localDir, err := loadLocalDir()
	if err != nil {
		return err
	}
	fileOp := files.NewFileOp()

	rootDir := fmt.Sprintf("%s/system/%s/%s", localDir, snap.Name, snap.Name)
	originalDir := fmt.Sprintf("%s/system/%s/original", localDir, snap.Name)
	if _, err := os.Stat(originalDir); err != nil && os.IsNotExist(err) {
		return fmt.Errorf("load original dir failed, err: %s", err)
	}
	_ = snapshotRepo.Update(snap.ID, map[string]interface{}{"rollback_status": constant.StatusWaiting})
	snapJson, err := u.readFromJson(fmt.Sprintf("%s/snapshot.json", rootDir))
	if err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, fmt.Sprintf("decompress file failed, err: %v", err))
		return err
	}

	_, _ = cmd.Exec("systemctl stop docker")
	if err := u.handleDockerDatas(fileOp, "rollback", originalDir, snapJson.OldDockerDataDir); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "DockerDir" {
		_, _ = cmd.Exec("systemctl restart docker")
		return nil
	}

	if err := u.handleDaemonJson(fileOp, "rollback", originalDir+"/daemon.json", ""); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "DaemonJson" {
		_, _ = cmd.Exec("systemctl restart docker")
		return nil
	}
	if snapJson.LiveRestoreEnabled {
		if err := u.updateLiveRestore(true); err != nil {
			updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
			return err
		}
	}
	if snap.InterruptStep == "UpdateLiveRestore" {
		_, _ = cmd.Exec("systemctl daemon-reload")
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		return nil
	}

	if err := u.handlePanelBinary(fileOp, "rollback", originalDir+"/1panel", ""); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "1PanelBinary" {
		_, _ = cmd.Exec("systemctl daemon-reload")
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		return nil
	}

	if err := u.handlePanelctlBinary(fileOp, "rollback", originalDir+"/1pctl", ""); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "1PctlBinary" {
		_, _ = cmd.Exec("systemctl daemon-reload")
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		return nil
	}

	if err := u.handlePanelService(fileOp, "rollback", originalDir+"/1panel.service", ""); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "1PanelService" {
		_, _ = cmd.Exec("systemctl daemon-reload")
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		return nil
	}

	if err := u.handleBackupDatas(fileOp, "rollback", originalDir, snapJson.OldBackupDataDir); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "1PanelBackups" {
		_, _ = cmd.Exec("systemctl daemon-reload")
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		return nil
	}

	if err := u.handlePanelDatas(fileOp, "rollback", originalDir, snapJson.OldPanelDataDir, "", ""); err != nil {
		updateRollbackStatus(snap.ID, constant.StatusFailed, err.Error())
		return err
	}
	if snap.InterruptStep == "1PanelData" {
		_, _ = cmd.Exec("systemctl daemon-reload")
		_, _ = cmd.Exec("systemctl restart 1panel.service")
		return nil
	}

	fmt.Println(000)
	_ = os.RemoveAll(rootDir)
	fmt.Println(111)
	global.LOG.Info("rollback successful")
	fmt.Println(222)
	_, _ = cmd.Exec("systemctl daemon-reload")
	fmt.Println(333)
	_, _ = cmd.Exec("systemctl restart 1panel.service")
	fmt.Println(444)
	updateRollbackStatus(snap.ID, constant.StatusSuccess, "")
	fmt.Println(555)
	return nil
}

func (u *SnapshotService) saveJson(snapJson SnapshotJson, path string) error {
	remarkInfo, _ := json.MarshalIndent(snapJson, "", "\t")
	if err := ioutil.WriteFile(fmt.Sprintf("%s/snapshot.json", path), remarkInfo, 0640); err != nil {
		return err
	}
	return nil
}

func (u *SnapshotService) readFromJson(path string) (SnapshotJson, error) {
	var snap SnapshotJson
	if _, err := os.Stat(path); err != nil {
		return snap, fmt.Errorf("find snapshot json file in recover package failed, err: %v", err)
	}
	fileByte, err := os.ReadFile(path)
	if err != nil {
		return snap, fmt.Errorf("read file from path %s failed, err: %v", path, err)
	}
	if err := json.Unmarshal(fileByte, &snap); err != nil {
		return snap, fmt.Errorf("unmarshal snapjson failed, err: %v", err)
	}
	return snap, nil
}

func (u *SnapshotService) handleDockerDatas(fileOp files.FileOp, operation string, source, target string) error {
	switch operation {
	case "snapshot":
		if err := u.handleTar(source, target, "docker_data.tar.gz", ""); err != nil {
			return fmt.Errorf("backup docker data failed, err: %v", err)
		}
	case "recover":
		if err := u.handleTar(target, fmt.Sprintf("%s/original", filepath.Join(source, "../")), "docker_data.tar.gz", ""); err != nil {
			return fmt.Errorf("backup docker data failed, err: %v", err)
		}
		if err := u.handleUnTar(source+"/docker/docker_data.tar.gz", target); err != nil {
			return fmt.Errorf("recover docker data failed, err: %v", err)
		}
	case "re-recover":
		if err := u.handleUnTar(source+"/docker/docker_data.tar.gz", target); err != nil {
			return fmt.Errorf("re-recover docker data failed, err: %v", err)
		}
	case "rollback":
		if err := u.handleUnTar(source+"/docker_data.tar.gz", target); err != nil {
			return fmt.Errorf("rollback docker data failed, err: %v", err)
		}
	}
	global.LOG.Info("handle docker data dir successful!")
	return nil
}

func (u *SnapshotService) handleDaemonJson(fileOp files.FileOp, operation string, source, target string) error {
	daemonJsonPath := "/etc/docker/daemon.json"
	if operation == "snapshot" || operation == "recover" {
		_, err := os.Stat(daemonJsonPath)
		if os.IsNotExist(err) {
			global.LOG.Info("no daemon.josn in snapshot and system now, nothing happened")
		}
		if err == nil {
			if err := fileOp.CopyFile(daemonJsonPath, target); err != nil {
				return fmt.Errorf("backup docker daemon.json failed, err: %v", err)
			}
		}
	}
	if operation == "recover" || operation == "rollback" || operation == "re-recover" {
		_, sourceErr := os.Stat(source)
		if os.IsNotExist(sourceErr) {
			_ = os.Remove(daemonJsonPath)
		}
		if sourceErr == nil {
			if err := fileOp.CopyFile(source, "/etc/docker"); err != nil {
				return fmt.Errorf("recover docker daemon.json failed, err: %v", err)
			}
		}
	}
	global.LOG.Info("handle docker daemon.json successful!")
	return nil
}

func (u *SnapshotService) handlePanelBinary(fileOp files.FileOp, operation string, source, target string) error {
	panelPath := "/usr/local/bin/1panel"
	if operation == "snapshot" || operation == "recover" {
		if _, err := os.Stat(panelPath); err != nil {
			return fmt.Errorf("1panel binary is not found in %s, err: %v", panelPath, err)
		} else {
			if err := cpBinary(panelPath, target); err != nil {
				return fmt.Errorf("backup 1panel binary failed, err: %v", err)
			}
		}
	}
	if operation == "recover" || operation == "rollback" || operation == "re-recover" {
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("1panel binary is not found in snapshot, err: %v", err)
		} else {
			if err := cpBinary(source, "/usr/local/bin/1panel"); err != nil {
				return fmt.Errorf("recover 1panel binary failed, err: %v", err)
			}
		}
	}
	global.LOG.Info("handle binary panel successful!")
	return nil
}
func (u *SnapshotService) handlePanelctlBinary(fileOp files.FileOp, operation string, source, target string) error {
	panelctlPath := "/usr/local/bin/1pctl"
	if operation == "snapshot" || operation == "recover" {
		if _, err := os.Stat(panelctlPath); err != nil {
			return fmt.Errorf("1pctl binary is not found in %s, err: %v", panelctlPath, err)
		} else {
			if err := cpBinary(panelctlPath, target); err != nil {
				return fmt.Errorf("backup 1pctl binary failed, err: %v", err)
			}
		}
	}
	if operation == "recover" || operation == "rollback" || operation == "re-recover" {
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("1pctl binary is not found in snapshot, err: %v", err)
		} else {
			if err := cpBinary(source, "/usr/local/bin/1pctl"); err != nil {
				return fmt.Errorf("recover 1pctl binary failed, err: %v", err)
			}
		}
	}
	global.LOG.Info("handle binary 1pactl successful!")
	return nil
}

func (u *SnapshotService) handlePanelService(fileOp files.FileOp, operation string, source, target string) error {
	panelServicePath := "/etc/systemd/system/1panel.service"
	if operation == "snapshot" || operation == "recover" {
		if _, err := os.Stat(panelServicePath); err != nil {
			return fmt.Errorf("1panel service is not found in %s, err: %v", panelServicePath, err)
		} else {
			if err := cpBinary(panelServicePath, target); err != nil {
				return fmt.Errorf("backup 1panel service failed, err: %v", err)
			}
		}
	}
	if operation == "recover" || operation == "rollback" || operation == "re-recover" {
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("1panel service is not found in snapshot, err: %v", err)
		} else {
			if err := cpBinary(source, "/etc/systemd/system/1panel.service"); err != nil {
				return fmt.Errorf("recover 1panel service failed, err: %v", err)
			}
		}
	}
	global.LOG.Info("handle panel service successful!")
	return nil
}

func (u *SnapshotService) handleBackupDatas(fileOp files.FileOp, operation string, source, target string) error {
	switch operation {
	case "snapshot":
		if err := u.handleTar(source, target, "1panel_backup.tar.gz", "./system"); err != nil {
			return fmt.Errorf("backup panel local backup dir data failed, err: %v", err)
		}
	case "recover":
		if err := u.handleTar(target, fmt.Sprintf("%s/original", filepath.Join(source, "../")), "1panel_backup.tar.gz", "./system"); err != nil {
			return fmt.Errorf("restore original local backup dir data failed, err: %v", err)
		}
		if err := u.handleUnTar(source+"/1panel/1panel_backup.tar.gz", target); err != nil {
			return fmt.Errorf("recover local backup dir data failed, err: %v", err)
		}
	case "re-recover":
		if err := u.handleUnTar(source+"/1panel/1panel_backup.tar.gz", target); err != nil {
			return fmt.Errorf("retry recover  local backup dir data failed, err: %v", err)
		}
	case "rollback":
		if err := u.handleUnTar(source+"/1panel_backup.tar.gz", target); err != nil {
			return fmt.Errorf("rollback local backup dir data failed, err: %v", err)
		}
	}
	global.LOG.Info("handle backup data successful!")
	return nil
}

func (u *SnapshotService) handlePanelDatas(fileOp files.FileOp, operation string, source, target, backupDir, dockerDir string) error {
	switch operation {
	case "snapshot":
		exclusionRules := ""
		if strings.Contains(backupDir, source) {
			exclusionRules += ("." + strings.ReplaceAll(backupDir, source, "") + ";")
		}
		if strings.Contains(dockerDir, source) {
			exclusionRules += ("." + strings.ReplaceAll(dockerDir, source, "") + ";")
		}
		if err := u.handleTar(source, target, "1panel_data.tar.gz", exclusionRules); err != nil {
			return fmt.Errorf("backup panel data failed, err: %v", err)
		}
	case "recover":
		exclusionRules := ""
		if strings.Contains(backupDir, target) {
			exclusionRules += ("1Panel" + strings.ReplaceAll(backupDir, target, "") + ";")
		}
		if strings.Contains(dockerDir, target) {
			exclusionRules += ("1Panel" + strings.ReplaceAll(dockerDir, target, "") + ";")
		}
		if err := u.handleTar(target, fmt.Sprintf("%s/original", filepath.Join(source, "../")), "1panel_data.tar.gz", exclusionRules); err != nil {
			return fmt.Errorf("restore original panel data failed, err: %v", err)
		}

		if err := u.handleUnTar(source+"/1panel/1panel_data.tar.gz", target); err != nil {
			return fmt.Errorf("recover panel data failed, err: %v", err)
		}
	case "re-recover":
		if err := u.handleUnTar(source+"/1panel/1panel_data.tar.gz", target); err != nil {
			return fmt.Errorf("retry recover panel data failed, err: %v", err)
		}
	case "rollback":
		if err := u.handleUnTar(source+"/1panel_data.tar.gz", target); err != nil {
			return fmt.Errorf("rollback panel data failed, err: %v", err)
		}
	}

	global.LOG.Info("handle panel data successful!")
	return nil
}

func (u *SnapshotService) loadDockerDataDir() (string, bool, error) {
	client, err := docker.NewDockerClient()
	if err != nil {
		return "", false, fmt.Errorf("new docker client failed, err: %v", err)
	}
	info, err := client.Info(context.Background())
	if err != nil {
		return "", false, fmt.Errorf("load docker info failed, err: %v", err)
	}
	return info.DockerRootDir, info.LiveRestoreEnabled, nil
}

func (u *SnapshotService) Delete(req dto.BatchDeleteReq) error {
	backups, _ := snapshotRepo.GetList(commonRepo.WithIdsIn(req.Ids))
	localDir, err := loadLocalDir()
	if err != nil {
		return err
	}
	for _, snap := range backups {
		if _, err := os.Stat(fmt.Sprintf("%s/system/%s/%s.tar.gz", localDir, snap.Name, snap.Name)); err == nil {
			_ = os.Remove(fmt.Sprintf("%s/system/%s/%s.tar.gz", localDir, snap.Name, snap.Name))
		}
	}
	if err := snapshotRepo.Delete(commonRepo.WithIdsIn(req.Ids)); err != nil {
		return err
	}

	return nil
}

func updateSnapshotStatus(id uint, status string, message string) {
	if status != constant.StatusSuccess {
		global.LOG.Errorf("snapshot failed, err: %s", message)
	}
	if err := snapshotRepo.Update(id, map[string]interface{}{
		"status":  status,
		"message": message,
	}); err != nil {
		global.LOG.Errorf("update snap snapshot status failed, err: %v", err)
	}
}
func updateRecoverStatus(id uint, interruptStep, status string, message string) {
	if status != constant.StatusSuccess {
		global.LOG.Errorf("recover failed, err: %s", message)
	}
	if err := snapshotRepo.Update(id, map[string]interface{}{
		"interrupt_step":    interruptStep,
		"recover_status":    status,
		"recover_message":   message,
		"last_recovered_at": time.Now().Format("2006-01-02 15:04:05"),
	}); err != nil {
		global.LOG.Errorf("update snap recover status failed, err: %v", err)
	}
}
func updateRollbackStatus(id uint, status string, message string) {
	if status == constant.StatusSuccess {
		if err := snapshotRepo.Update(id, map[string]interface{}{
			"recover_status":     "",
			"recover_message":    "",
			"interrupt_step":     "",
			"rollback_status":    "",
			"rollback_message":   "",
			"last_rollbacked_at": time.Now().Format("2006-01-02 15:04:05"),
		}); err != nil {
			global.LOG.Errorf("update snap recover status failed, err: %v", err)
		}
		return
	}
	global.LOG.Errorf("rollback failed, err: %s", message)
	if err := snapshotRepo.Update(id, map[string]interface{}{
		"rollback_status":    status,
		"rollback_message":   message,
		"last_rollbacked_at": time.Now().Format("2006-01-02 15:04:05"),
	}); err != nil {
		global.LOG.Errorf("update snap recover status failed, err: %v", err)
	}
}

func cpBinary(src, dst string) error {
	stderr, err := cmd.Exec(fmt.Sprintf("\\cp -f %s %s", src, dst))
	if err != nil {
		return errors.New(stderr)
	}
	return nil
}

func (u *SnapshotService) updateLiveRestore(enabled bool) error {
	if _, err := os.Stat(constant.DaemonJsonPath); err != nil {
		return fmt.Errorf("load docker daemon.json conf failed, err: %v", err)
	}
	file, err := ioutil.ReadFile(constant.DaemonJsonPath)
	if err != nil {
		return err
	}
	deamonMap := make(map[string]interface{})
	_ = json.Unmarshal(file, &deamonMap)

	if !enabled {
		delete(deamonMap, "live-restore")
	} else {
		deamonMap["live-restore"] = enabled
	}
	newJson, err := json.MarshalIndent(deamonMap, "", "\t")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(constant.DaemonJsonPath, newJson, 0640); err != nil {
		return err
	}

	stdout, err := cmd.Exec("systemctl restart docker")
	if err != nil {
		return errors.New(stdout)
	}
	time.Sleep(10 * time.Second)
	return nil
}

func (u *SnapshotService) handleTar(sourceDir, targetDir, name, exclusionRules string) error {
	if _, err := os.Stat(targetDir); err != nil && os.IsNotExist(err) {
		if err = os.MkdirAll(targetDir, os.ModePerm); err != nil {
			return err
		}
	}
	exStr := []string{"--warning=no-file-changed"}
	exStr = append(exStr, "-zcf")
	exStr = append(exStr, targetDir+"/"+name)
	excludes := strings.Split(exclusionRules, ";")
	for _, exclude := range excludes {
		if len(exclude) == 0 {
			continue
		}
		exStr = append(exStr, "--exclude")
		exStr = append(exStr, exclude)
	}
	exStr = append(exStr, "-C")
	exStr = append(exStr, sourceDir)
	exStr = append(exStr, ".")
	cmd := exec.Command("tar", exStr...)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(stdout))
	}
	return nil
}

func (u *SnapshotService) handleUnTar(sourceDir, targetDir string) error {
	if _, err := os.Stat(targetDir); err != nil && os.IsNotExist(err) {
		if err = os.MkdirAll(targetDir, os.ModePerm); err != nil {
			return err
		}
	}
	exStr := []string{}
	exStr = append(exStr, "zxf")
	exStr = append(exStr, sourceDir)
	exStr = append(exStr, "-C")
	exStr = append(exStr, targetDir)
	exStr = append(exStr, ".")
	cmd := exec.Command("tar", exStr...)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(stdout))
	}
	return nil
}