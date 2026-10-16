#!/usr/bin/env python
# -*- coding: utf-8 -*-

# Copyright 2018-2022 Mailgun Technologies Inc
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

from setuptools import setup, find_packages

# requirements-py3.txt is also the dev environment; installing the client needs only the
# runtime packages, with the tested versions as minimums.
DEV_ONLY = ('pytest==', 'grpcio-tools==')

with open('requirements-py3.txt', 'r') as requirements_file:
    requirements = [line.strip().replace('==', '>=') for line in requirements_file
                    if line.strip() and not line.startswith(DEV_ONLY)]

setup(
    name='gubernator',
    version='0.1.0',
    description="Python client for gubernator",
    author="Derrick J. Wippler",
    author_email='thrawn01@gmail.com',
    url='https://github.com/gubernator-io/gubernator',
    package_dir={'': '.'},
    packages=find_packages('.', exclude=['tests']),
    install_requires=requirements,
    license="Apache Software License 2.0",
    python_requires='>=3.10',
    classifiers=[
        'Development Status :: 4 - Beta',
        'Intended Audience :: Developers',
        'License :: OSI Approved :: Apache Software License',
        'Natural Language :: English',
        'Programming Language :: Python :: 3.10',
        'Programming Language :: Python :: 3.11',
        'Programming Language :: Python :: 3.12',
        'Programming Language :: Python :: 3.13',
    ],
)
