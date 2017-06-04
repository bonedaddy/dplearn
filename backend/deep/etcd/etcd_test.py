"""
This script tests etcd clients.
"""

from __future__ import print_function

import os.path
import sys
import time
import shutil
import subprocess
import threading
import unittest
from etcd import put
from etcd import get

class ETCD(threading.Thread):
    """
    wraps etcd subprocess
    """
    def __init__(self, ETCD_PATH):
        self.stdout = None
        self.stderr = None
        self.process = None
        self.exec_path = ETCD_PATH
        threading.Thread.__init__(self)

    def run(self):
        self.process = subprocess.Popen([
            self.exec_path,
            '--name', 's1',
            '--data-dir', 'etcd-test-data',
            '--listen-client-urls', 'http://localhost:2379',
            '--advertise-client-urls', 'http://localhost:2379',
            '--listen-peer-urls', 'http://localhost:2380',
            '--initial-advertise-peer-urls', 'http://localhost:2380',
            '--initial-cluster', 's1=http://localhost:2380',
            '--initial-cluster-token', 'mytoken',
            '--initial-cluster-state', 'new',
            '--auto-compaction-retention', '1',
        ], shell=False, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

        self.stdout, self.stderr = self.process.communicate()

    def kill(self):
        """
        Kills the running etcd process
        """
        self.process.kill()

class TestETCDMethods(unittest.TestCase):
    """
    etcd testing methods
    """
    def test_etcd(self):
        """
        etcd test function
        """
        exec_path = os.environ['ETCD_TEST_PATH']
        if exec_path == '':
            print('Got empty etcd path')
            sys.exit(0)
        if os.path.exists(exec_path) != True:
            print('{0} does not eixst'.format(exec_path))
            sys.exit(0)

        print('Running {0}'.format(exec_path))
        etcd_proc = ETCD(exec_path)
        etcd_proc.setDaemon(True)
        etcd_proc.start()

        print('Sleeping...')
        time.sleep(5)

        print('Launching client requests...')
        print(put('http://localhost:2379', 'foo', 'bar'))
        self.assertEqual(get('http://localhost:2379', 'foo'), 'bar')

        print('Killing etcd...')
        etcd_proc.kill()

        etcd_proc.join()
        print('etcd output: {0}'.format(etcd_proc.stderr))

        print('Removing etcd data directory...')
        shutil.rmtree('etcd-test-data')
        print('Done!')

if __name__ == '__main__':
    unittest.main()
